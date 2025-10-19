package main

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	flag "github.com/spf13/pflag"

	"github.com/rwcarlsen/goexif/exif"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

func main() {
	inPath := flag.StringP("in", "i", ".", "input image path or directory (jpg/png)")
	outPath := flag.StringP("out", "o", ".", "output image path (optional, only for single file)")
	marginPercent := flag.IntP("margin", "m", 5, "margin from edges as percentage of the smaller image dimension")
	recursive := flag.BoolP("recursive", "r", false, "when input is a directory, recurse into subdirectories")
	fontPath := flag.StringP("font", "f", "arial.ttf", "path to .ttf font file to use for watermark (optional)")
	widthPercent := flag.IntP("widthpercent", "w", 40, "watermark max width as percentage of image width (1-100)")
	rename := flag.BoolP("rename", "n", false, "rename output file to EXIF capture time (as filename)")
	concurrency := flag.IntP("concurrency", "c", runtime.NumCPU(), "number of concurrent workers when processing a directory")
	help := flag.BoolP("help", "?", false, "display help")
	flag.Parse()
	if *help {
		flag.Usage()
		return
	}

	// context for graceful shutdown on Ctrl+C
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Printf("signal received, shutting down...")
		cancel()
	}()

	// If user passed a bare font filename (e.g. "arial.ttf"), try to find it in system font dirs
	if *fontPath != "" {
		if filepath.Base(*fontPath) == *fontPath && !filepath.IsAbs(*fontPath) {
			if p := findSystemFont(*fontPath); p != "" {
				*fontPath = p
			}
		}
	}

	// parse and cache TTF font once (so we don't re-read/parse for every image)
	var parsedFont *opentype.Font
	if *fontPath != "" {
		if b, err := os.ReadFile(*fontPath); err == nil {
			if ft, err := opentype.Parse(b); err == nil {
				parsedFont = ft
			} else {
				log.Printf("warning: failed to parse font %s: %v", *fontPath, err)
			}
		} else {
			log.Printf("warning: failed to read font %s: %v", *fontPath, err)
		}
	}

	if *inPath == "" {
		log.Fatalf("missing -in parameter\nUsage: %s -in photo.jpg|dir [-out out.jpg] [-recursive]", os.Args[0])
	}

	outIsDir := false

	// Determine if input is dir or file
	fi, err := os.Stat(*inPath)
	if err != nil {
		log.Fatalf("stat input: %v", err)
	}

	if fi.IsDir() {
		// if input is a directory, always treat outPath as a directory
		// (no need for user to append a trailing separator)
		if *outPath == "" {
			*outPath = "."
		}
		outIsDir = true
		// create output dir if it doesn't exist
		if err := os.MkdirAll(*outPath, 0755); err != nil {
			log.Fatalf("create out dir: %v", err)
		}
		// walk directory and collect images to process
		var files []string
		walkFn := func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}

			// stop walking if context cancelled
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if d.IsDir() {
				if path == *inPath {
					return nil
				}
				if !*recursive {
					return filepath.SkipDir
				}
				return nil
			}
			ext := filepath.Ext(d.Name())
			low := ext
			if len(low) > 0 {
				low = low[1:]
			}
			switch strings.ToLower(low) {
			case "jpg", "jpeg", "png":
				files = append(files, path)
			}
			return nil
		}
		// WalkDir: record errors encountered during traversal but continue where possible
		if err := filepath.WalkDir(*inPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				log.Printf("walk error %s: %v", path, err)
				return nil
			}
			return walkFn(path, d, nil)
		}); err != nil {
			if err == context.Canceled {
				log.Printf("walk cancelled")
				// proceed with whatever files were collected
			} else {
				log.Fatalf("walkdir failed: %v", err)
			}
		}

		// If no files found, exit
		if len(files) == 0 {
			fmt.Println("no images found")
			return
		}

		// Worker pool to process files concurrently, respond to cancellation
		jobs := make(chan string)
		// determine number of workers
		n := *concurrency
		if n <= 0 {
			n = 1
		}
		// buffered results channel reduces the risk of worker goroutines blocking
		results := make(chan struct {
			out string
			err error
		}, n*2)
		var wg sync.WaitGroup

		worker := func() {
			defer wg.Done()
			for p := range jobs {
				// respect cancellation
				select {
				case <-ctx.Done():
					results <- struct {
						out string
						err error
					}{"", ctx.Err()}
					return
				default:
				}
				// construct out path preserving relative structure
				rel, err := filepath.Rel(*inPath, p)
				if err != nil {
					rel = filepath.Base(p)
				}
				relDir := filepath.Dir(rel)
				destDir := filepath.Join(*outPath, relDir)
				if err := os.MkdirAll(destDir, 0755); err != nil {
					results <- struct {
						out string
						err error
					}{"", fmt.Errorf("mkdir dest: %w", err)}
					continue
				}
				ext := filepath.Ext(p)
				base := fileBase(p)
				out := filepath.Join(destDir, fmt.Sprintf("%s_watermarked%s", base, ext))
				outFile, err := processImage(p, out, *marginPercent, parsedFont, *widthPercent, *rename)
				results <- struct {
					out string
					err error
				}{outFile, err}
			}
		}

		// start workers
		wg.Add(n)
		for i := 0; i < n; i++ {
			go worker()
		}

		// dispatch jobs; stop dispatching if cancelled
		go func() {
			defer close(jobs)
			for _, p := range files {
				select {
				case <-ctx.Done():
					return
				default:
				}
				jobs <- p
			}
		}()

		// close results when all workers finish
		go func() {
			wg.Wait()
			close(results)
		}()

		// collect results until workers are done or cancelled
		for res := range results {
			if res.err != nil {
				log.Printf("process: %v", res.err)
			} else {
				fmt.Printf("wrote %s\n", res.out)
			}
		}
		return
	}

	// single file
	out := *outPath
	if out == "" {
		ext := filepath.Ext(*inPath)
		name := (*inPath)[:len(*inPath)-len(ext)]
		out = fmt.Sprintf("%s_watermarked%s", name, ext)
	} else if outIsDir {
		// place output inside specified directory
		ext := filepath.Ext(*inPath)
		base := fileBase(*inPath)
		os.MkdirAll(out, 0755)
		out = filepath.Join(out, fmt.Sprintf("%s_watermarked%s", base, ext))
	}
	if outFile, err := processImage(*inPath, out, *marginPercent, parsedFont, *widthPercent, *rename); err != nil {
		log.Fatalf("process image: %v", err)
	} else {
		fmt.Printf("wrote %s\n", outFile)
	}
}

// helper: lowercase ascii
// using strings.ToLower from stdlib

func fileBase(path string) string {
	name := filepath.Base(path)
	ext := filepath.Ext(name)
	if ext == "" {
		return name
	}
	return name[:len(name)-len(ext)]
}

// processImage reads input, extracts date, wraps text if needed, draws multi-line watermark, and writes output
// processImage reads input, extracts date, draws watermark, and writes output.
// If rename is true, the output filename (inside outPath's directory) will be replaced
// with a safe filename derived from the EXIF capture time.
// Returns the actual written output path on success.
func processImage(inPath, outPath string, marginPercent int, fontFT *opentype.Font, widthPercent int, rename bool) (string, error) {
	// Open file once and use stream for EXIF and image decoding to avoid reading whole file into memory
	f, err := os.Open(inPath)
	if err != nil {
		return "", fmt.Errorf("open input: %w", err)
	}
	defer f.Close()

	// Try to read EXIF
	dateStr := ""
	if ex, err := exif.Decode(f); err == nil {
		if tag, err := ex.Get(exif.DateTimeOriginal); err == nil && tag != nil {
			if s, err := tag.StringVal(); err == nil {
				dateStr = s
			}
		}
		if dateStr == "" {
			if tag, err := ex.Get(exif.DateTime); err == nil && tag != nil {
				if s, err := tag.StringVal(); err == nil {
					dateStr = s
				}
			}
		}
	}

	// Fallback to file mod time
	if dateStr == "" {
		if fi, err := os.Stat(inPath); err == nil {
			dateStr = fi.ModTime().Format("2006-01-02 15:04:05")
		} else {
			dateStr = time.Now().Format("2006-01-02 15:04:05")
		}
	}
	dateStr = normalizeExifDate(dateStr)

	// seek back to beginning for image decoding
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek input: %w", err)
	}

	img, format, err := image.Decode(f)
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}

	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)

	// determine font face: if a parsed TTF font is provided, choose size so that text width <= widthPercent% of image width
	var face font.Face
	var drawer *font.Drawer
	imgWidth := bounds.Dx()
	availableWidth := max(imgWidth*widthPercent/100, 10)

	if fontFT != nil {
		// binary search font size in points
		lo := 4.0
		hi := float64(imgWidth) // arbitrary upper bound
		var chosen font.Face
		for i := 0; i < 12; i++ {
			mid := (lo + hi) / 2
			f, err := opentype.NewFace(fontFT, &opentype.FaceOptions{Size: mid, DPI: 72})
			if err != nil {
				hi = mid
				continue
			}
			tmpDrawer := &font.Drawer{Dst: rgba, Src: image.NewUniform(color.RGBA{255, 255, 255, 220}), Face: f}
			lines := wrapText(tmpDrawer, dateStr, availableWidth)
			maxW := 0
			for _, L := range lines {
				w := tmpDrawer.MeasureString(L).Ceil()
				if w > maxW {
					maxW = w
				}
			}
			if maxW <= availableWidth {
				chosen = f
				lo = mid
			} else {
				hi = mid
			}
		}
		if chosen != nil {
			face = chosen
			drawer = &font.Drawer{Dst: rgba, Src: image.NewUniform(color.RGBA{255, 255, 255, 220}), Face: face}
		}
	}
	if drawer == nil {
		face = basicfont.Face7x13
		drawer = &font.Drawer{Dst: rgba, Src: image.NewUniform(color.RGBA{255, 255, 255, 220}), Face: face}
	}

	lines := wrapText(drawer, dateStr, availableWidth)

	metrics := face.Metrics()
	ascent := metrics.Ascent.Ceil()
	descent := metrics.Descent.Ceil()
	lineHeight := ascent + descent
	// convert marginPercent to pixel margin using the smaller image dimension
	imgHeight := bounds.Dy()
	smaller := min(imgWidth, imgHeight)
	pixelMargin := max(smaller*marginPercent/100, 1)

	// starting y for the first (top) line of the block so that block bottom is pixelMargin above bottom
	startY := max(bounds.Max.Y-pixelMargin-descent-(len(lines)-1)*lineHeight, ascent+pixelMargin)

	// draw each line right-aligned
	for i, line := range lines {
		textWidth := drawer.MeasureString(line).Ceil()
		x := max(bounds.Max.X-textWidth-pixelMargin, pixelMargin)
		y := startY + i*lineHeight

		// draw white outline by drawing the text multiple times around the center
		// outline thickness scales with font size
		outlinePx := max(lineHeight/20, 1)
		drawerOrig := *drawer
		for ox := -outlinePx; ox <= outlinePx; ox++ {
			for oy := -outlinePx; oy <= outlinePx; oy++ {
				// skip center (will be drawn as main text)
				if ox == 0 && oy == 0 {
					continue
				}
				d := drawerOrig
				d.Src = image.NewUniform(color.RGBA{255, 255, 255, 255})
				d.Dot = fixed.P(x+ox, y+oy)
				d.DrawString(line)
			}
		}

		// main fill (black)
		drawer.Src = image.NewUniform(color.RGBA{0, 0, 0, 255})
		drawer.Dot = fixed.P(x, y)
		drawer.DrawString(line)
	}

	// determine final output path
	ext := filepath.Ext(outPath)
	outDir := filepath.Dir(outPath)
	finalOut := outPath
	if rename {
		// build safe filename from dateStr: replace spaces with '_' and ':' with '-'
		dateForFile := strings.ReplaceAll(dateStr, " ", "_")
		dateForFile = strings.ReplaceAll(dateForFile, ":", "-")
		dateForFile = safeFilename(dateForFile)
		if dateForFile == "" {
			dateForFile = "unknown_date"
		}
		candidate := filepath.Join(outDir, dateForFile+ext)
		// if exists, add numeric suffix
		finalOut = uniquePath(candidate)
	} else {
		finalOut = uniquePath(outPath)
	}

	of, err := os.Create(finalOut)
	if err != nil {
		return "", fmt.Errorf("create output: %w", err)
	}
	defer of.Close()

	switch format {
	case "png":
		if err := png.Encode(of, rgba); err != nil {
			return "", fmt.Errorf("encode png: %w", err)
		}
	default:
		opts := &jpeg.Options{Quality: 95}
		if err := jpeg.Encode(of, rgba, opts); err != nil {
			return "", fmt.Errorf("encode jpeg: %w", err)
		}
	}
	// Try to set file times to EXIF capture time on Windows
	if runtime.GOOS == "windows" {
		if t, err := parseExifTime(dateStr); err == nil {
			if err := os.Chtimes(finalOut, t, t); err != nil {
				log.Printf("failed to set file times for %s: %v", finalOut, err)
			}
		} else {
			log.Printf("failed to parse exif date '%s': %v", dateStr, err)
		}
	}

	return finalOut, nil
}

// parseExifTime tries several common layouts to parse the normalized EXIF date string.
func parseExifTime(s string) (time.Time, error) {
	// try common layouts
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02_15-04-05",
		"2006-01-02",
		time.RFC3339,
	}
	var lastErr error
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
}

// safeFilename replaces characters unsafe for filenames with underscores and keeps common safe chars.
func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		// allow letters, numbers, dash, underscore, and dot
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// uniquePath returns a path that does not exist by appending _1, _2, ... when needed.
func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	dir := filepath.Dir(p)
	base := fileBase(p)
	ext := filepath.Ext(p)
	for i := 1; i < 10000; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, i, ext))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	// fallback: return original (will likely overwrite)
	return p
}

func normalizeExifDate(s string) string {
	// common EXIF date format: "2006:01:02 15:04:05"
	if len(s) >= 10 && s[4] == ':' && s[7] == ':' {
		// replace first two ':' with '-'
		runes := []rune(s)
		runes[4] = '-'
		runes[7] = '-'
		return string(runes)
	}
	return s
}

// wrapText splits text into lines so each line fits within maxWidth (pixels) using the provided drawer.
func wrapText(drawer *font.Drawer, text string, maxWidth int) []string {
	// simple greedy wrap by spaces; if a word is too long, break by characters
	words := strings.Fields(text)
	var lines []string
	if len(words) == 0 {
		return []string{""}
	}
	cur := words[0]
	for i := 1; i < len(words); i++ {
		w := words[i]
		try := cur + " " + w
		if drawer.MeasureString(try).Ceil() <= maxWidth {
			cur = try
		} else {
			// current line full, push it
			lines = append(lines, cur)
			// start new line with w, but if w alone too long, break it
			if drawer.MeasureString(w).Ceil() <= maxWidth {
				cur = w
			} else {
				// break word into chars
				part := ""
				for _, ch := range w {
					try2 := part + string(ch)
					if drawer.MeasureString(try2).Ceil() <= maxWidth {
						part = try2
					} else {
						if part != "" {
							lines = append(lines, part)
						}
						part = string(ch)
					}
				}
				cur = part
			}
		}
	}
	lines = append(lines, cur)
	return lines
}

// findSystemFont searches common system font directories for the given filename (case-insensitive)
func findSystemFont(filename string) string {
	var dirs []string
	switch runtime.GOOS {
	case "windows":
		dirs = []string{"C:\\Windows\\Fonts"}
	case "darwin":
		dirs = []string{"/System/Library/Fonts", "/Library/Fonts", filepath.Join(os.Getenv("HOME"), "Library/Fonts")}
	default:
		// linux/unix
		dirs = []string{"/usr/share/fonts", "/usr/local/share/fonts", filepath.Join(os.Getenv("HOME"), ".fonts")}
	}

	lower := strings.ToLower(filename)
	for _, d := range dirs {
		fpath := filepath.Join(d, filename)
		if _, err := os.Stat(fpath); err == nil {
			return fpath
		}
		// try case-insensitive scan
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.ToLower(e.Name()) == lower {
				return filepath.Join(d, e.Name())
			}
		}
	}
	return ""
}
