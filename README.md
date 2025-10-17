# Water - 照片日期水印工具

这是一个简单的 Go 程序，用于读取照片 EXIF 中的拍摄日期（DateTimeOriginal / DateTime），并将日期作为水印绘制在照片的右下角。

用法：

- 构建：
  go build

- 运行：
  go run . -in photo.jpg [-out photo_watermarked.jpg] [-margin 12]

参数：
- -in: 输入图片路径（必需），支持 JPEG / PNG
- -out: 输出图片路径（可选），默认将会在原文件名后加 `_watermarked`
- -margin: 水印距离图片边缘的像素（默认 12）

示例：

  go run . -in DSC01234.JPG -out DSC01234_marked.jpg

注意：
- 若图片不含 EXIF 拍摄日期，程序会使用文件的修改时间作为替代。
- 使用了 `github.com/rwcarlsen/goexif` 和 `golang.org/x/image` 来解析 EXIF 并绘制文字。
