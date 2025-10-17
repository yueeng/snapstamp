package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

func main() {
	inPath := flag.String("in", "", "input image path or directory (jpg/png)")
	outPath := flag.String("out", "", "output image path (optional, only for single file)")
	margin := flag.Int("margin", 12, "margin from edges in pixels")
	recursive := flag.Bool("recursive", false, "when input is a directory, recurse into subdirectories")
	fontPath := flag.String("font", "", "path to .ttf font file to use for watermark (optional)")
	widthPercent := flag.Int("widthpercent", 30, "watermark max width as percentage of image width (1-100)")
	flag.Parse()

	// If user passed a bare font filename (e.g. "arial.ttf"), try to find it in system font dirs
	if *fontPath != "" {
		if filepath.Base(*fontPath) == *fontPath && !filepath.IsAbs(*fontPath) {
			if p := findSystemFont(*fontPath); p != "" {
				*fontPath = p
			}
		}
	}

	if *inPath == "" {
		log.Fatalf("missing -in parameter\nUsage: %s -in photo.jpg|dir [-out out.jpg] [-recursive]", os.Args[0])
	}

	// detect if user provided -out as a directory (existing dir or trailing separator)
	outIsDir := false
	if *outPath != "" {
		if st, err := os.Stat(*outPath); err == nil && st.IsDir() {
			outIsDir = true
		} else if strings.HasSuffix(*outPath, string(os.PathSeparator)) || strings.HasSuffix(*outPath, "/") {
			outIsDir = true
			// create if needed
			os.MkdirAll(*outPath, 0755)
		}
	}

	// Determine if input is dir or file
	fi, err := os.Stat(*inPath)
	if err != nil {
		log.Fatalf("stat input: %v", err)
	}

	if fi.IsDir() {
		// walk directory and process images
		walkFn := func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
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
			switch stringToLower(low) {
			case "jpg", "jpeg", "png":
				if outIsDir {
					// build relative output path under the output dir, preserving structure
					rel, err := filepath.Rel(*inPath, path)
					if err != nil {
						rel = filepath.Base(path)
					}
					relDir := filepath.Dir(rel)
					destDir := filepath.Join(*outPath, relDir)
					os.MkdirAll(destDir, 0755)
					base := fileBase(path)
					out := filepath.Join(destDir, fmt.Sprintf("%s_watermarked%s", base, ext))
					if err := processImage(path, out, *margin, *fontPath, *widthPercent); err != nil {
						log.Printf("process %s: %v", path, err)
					} else {
						fmt.Printf("wrote %s\n", out)
					}
				} else {
					out := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s_watermarked%s", fileBase(path), ext))
					if err := processImage(path, out, *margin, *fontPath, *widthPercent); err != nil {
						log.Printf("process %s: %v", path, err)
					} else {
						fmt.Printf("wrote %s\n", out)
					}
				}
			}
			return nil
		}
		filepath.WalkDir(*inPath, walkFn)
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
	if err := processImage(*inPath, out, *margin, *fontPath, *widthPercent); err != nil {
		log.Fatalf("process image: %v", err)
	}
	fmt.Printf("wrote %s\n", out)
}

// helper: lowercase ascii
func stringToLower(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] = b[i] + 32
		}
	}
	return string(b)
}

func fileBase(path string) string {
	name := filepath.Base(path)
	ext := filepath.Ext(name)
	return name[:len(name)-len(ext)]
}

// processImage reads input, extracts date, wraps text if needed, draws multi-line watermark, and writes output
func processImage(inPath, outPath string, margin int, fontPath string, widthPercent int) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	// Try to read EXIF
	dateStr := ""
	if ex, err := exif.Decode(bytes.NewReader(data)); err == nil {
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

	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}

	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)

	// determine font face: if TTF provided, choose size so that text width <= widthPercent% of image width
	var face font.Face
	var drawer *font.Drawer
	imgWidth := bounds.Dx()
	availableWidth := imgWidth * widthPercent / 100
	if availableWidth < 10 {
		availableWidth = 10
	}

	if fontPath != "" {
		b, err := os.ReadFile(fontPath)
		if err == nil {
			if ft, err := opentype.Parse(b); err == nil {
				// binary search font size in points
				lo := 4.0
				hi := float64(imgWidth) // arbitrary upper bound
				var chosen font.Face
				for iter := 0; iter < 20; iter++ {
					mid := (lo + hi) / 2
					f, err := opentype.NewFace(ft, &opentype.FaceOptions{Size: mid, DPI: 72})
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

	// starting y for the first (top) line of the block so that block bottom is margin above bottom
	startY := bounds.Max.Y - margin - descent - (len(lines)-1)*lineHeight
	if startY < ascent+margin {
		startY = ascent + margin
	}

	// draw each line right-aligned
	for i, line := range lines {
		textWidth := drawer.MeasureString(line).Ceil()
		x := bounds.Max.X - textWidth - margin
		if x < margin {
			x = margin
		}
		y := startY + i*lineHeight

		// shadow
		shadowDrawer := *drawer
		shadowDrawer.Src = image.NewUniform(color.RGBA{0, 0, 0, 200})
		shadowDrawer.Dot = fixed.P(x+1, y+1)
		shadowDrawer.DrawString(line)

		// main
		drawer.Src = image.NewUniform(color.RGBA{255, 255, 255, 230})
		drawer.Dot = fixed.P(x, y)
		drawer.DrawString(line)
	}

	of, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer of.Close()

	switch format {
	case "png":
		if err := png.Encode(of, rgba); err != nil {
			return fmt.Errorf("encode png: %w", err)
		}
	default:
		opts := &jpeg.Options{Quality: 95}
		if err := jpeg.Encode(of, rgba, opts); err != nil {
			return fmt.Errorf("encode jpeg: %w", err)
		}
	}
	return nil
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
	words := splitSpaces(text)
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

// splitSpaces splits on spaces, preserving chunks (simple)
func splitSpaces(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
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
