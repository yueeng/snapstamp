# SnapStamp - 照片日期水印工具

一个小巧的 Go 命令行工具，用于从照片 EXIF 中提取拍摄时间并在图片右下角绘制日期水印。支持单张图片处理与目录批量处理（可保留原始相对目录结构）。

快速上手

- 构建二进制（PowerShell）：

```powershell
go build -o snapstamp
```

- 直接运行（单图片）：

```powershell
go run . -in "C:\path\to\photo.jpg"
```

- 批量处理目录并输出到目标目录，保留相对结构，使用 8 并发：

```powershell
go run . -in "C:\photos\src" -out "C:\photos\dst" -concurrency 8
```

核心功能

- 从 EXIF 读取 DateTimeOriginal / DateTime；若缺失则回退到文件修改时间。
- 支持 JPG/JPEG/PNG。PNG 输入会输出为 PNG，其他格式按 JPEG 输出（质量 95）。
- 支持自定义 TTF 字体（传完整路径或只传文件名，程序会在常见系统字体目录尝试查找）。
- 字体大小按图片宽度自动缩放（受 `-widthpercent` 控制）。
- 可以将输出文件重命名为 EXIF 日期（使用 `-rename`）。

示例

- 单文件（输出为原名 + `_watermarked`）：

```powershell
go run . -in "photo.jpg"
```

- 单文件并指定输出：

```powershell
go run . -in "photo.jpg" -out "photo_out.jpg"
```

- 目录（递归）处理示例：

```powershell
go run . -in "C:\photos\src" -out "C:\photos\dst" -recursive -concurrency 4
```

- 使用系统字体（例如 `arial.ttf`）：

```powershell
go run . -in "photo.jpg" -font "arial.ttf"
```

重要参数说明

- -in string (必需)：输入文件或目录（支持 JPG/JPEG/PNG）。
- -out string：输出文件或目录（当输入为目录时应为目录）。
- -margin int：水印与图片边缘距离，按较短边的百分比计算，默认 5。
- -recursive bool：目录是否递归，默认 false。
- -font string：TTF 字体路径或文件名（例如 `arial.ttf`）。若只传文件名，程序会在系统字体目录查找；若失败回退到内置小字体。
- -widthpercent int：水印最大宽度占图片宽度百分比（1-100），默认 40。
- -rename bool：以 EXIF 日期重命名输出文件（若冲突会自动添加后缀）。
- -concurrency int：并发 worker 数，默认使用 CPU 核心数。

常见问题（FAQ）

- Q: 程序提示无法加载字体或加载失败，如何处理？
  - A: 建议传入字体完整路径（例如 `C:\Windows\Fonts\arial.ttf`）。如果只给文件名，程序会在系统字体目录（Windows: `C:\Windows\Fonts`）尝试查找。字体加载失败不会使处理停止，程序会回退到 `basicfont.Face7x13`。

- Q: 图片没有 EXIF 时间怎么办？
  - A: 程序会使用文件系统的修改时间作为回退；若也不可用则使用当前时间作为水印文本。

- Q: 想定制水印样式（颜色、半透明背景、阴影等），需要怎么改？
  - A: 目前内置的是白色描边 + 黑色填充。若要更多样式，我可以在 `main.go` 中添加参数（例如 `-color`, `-bg`, `-alpha`, `-shadow`）并实现这些样式。

- Q: 能否支持更多图片格式（WebP/HEIC）？
  - A: WebP 可以通过引入纯 Go 库支持。HEIC/HEIF 通常需要 `libheif` 及 cgo 支持（平台依赖），如果你需要我可以帮你调研并集成。

性能与故障排查

- 若处理大量大图像，建议将 `-concurrency` 设置得较低以减少同时内存占用。
- 若遇到 I/O 瓶颈，优先排查磁盘速度与并发数量。