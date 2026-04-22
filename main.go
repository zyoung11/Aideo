package main

import (
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// ==================== 终端控制 ====================

type TerminalSize struct {
	Width  int
	Height int
}

func getTerminalSize() (*TerminalSize, error) {
	type winsize struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}

	ws := &winsize{}
	retCode, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(syscall.Stdout),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(ws)))

	if int(retCode) == -1 {
		return nil, fmt.Errorf("syscall failed: %v", errno)
	}

	return &TerminalSize{
		Width:  int(ws.Col),
		Height: int(ws.Row),
	}, nil
}

const (
	CLEAR_SCREEN = "\033[2J"
	CURSOR_HOME  = "\033[H"
	HIDE_CURSOR  = "\033[?25l"
	SHOW_CURSOR  = "\033[?25h"
	RESET_COLORS = "\033[0m"
)

func clearScreen() {
	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)
}

func hideCursor() {
	fmt.Print(HIDE_CURSOR)
}

func showCursor() {
	fmt.Print(SHOW_CURSOR)
}

func colorFg(r, g, b uint8) string {
	return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
}

func colorBg(r, g, b uint8) string {
	return fmt.Sprintf("\033[48;2;%d;%d;%dm", r, g, b)
}

func colorFgBg(fgR, fgG, fgB, bgR, bgG, bgB uint8) string {
	return fmt.Sprintf("\033[38;2;%d;%d;%d;48;2;%d;%d;%dm",
		fgR, fgG, fgB, bgR, bgG, bgB)
}

// ==================== 图像数据结构 ====================

type ColorData struct {
	Width, Height int
	Gray          []float64
	R, G, B       []uint8
}

// ==================== 图像加载 ====================

func loadImage(filename string) (*ColorData, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("无法打开文件: %v", err)
	}
	defer file.Close()

	var img image.Image
	lowerName := strings.ToLower(filename)

	if strings.HasSuffix(lowerName, ".png") {
		img, err = png.Decode(file)
	} else if strings.HasSuffix(lowerName, ".jpg") || strings.HasSuffix(lowerName, ".jpeg") {
		img, err = jpeg.Decode(file)
	} else {
		return nil, fmt.Errorf("不支持的格式，请使用 JPG 或 PNG")
	}

	if err != nil {
		return nil, fmt.Errorf("解码图像失败: %v", err)
	}

	bounds := img.Bounds()
	width, height := bounds.Max.X, bounds.Max.Y
	total := width * height

	gray := make([]float64, total)
	r := make([]uint8, total)
	g := make([]uint8, total)
	b := make([]uint8, total)

	nrgbaModel := color.NRGBAModel

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := nrgbaModel.Convert(img.At(x, y)).(color.NRGBA)
			idx := y*width + x

			r[idx] = c.R
			g[idx] = c.G
			b[idx] = c.B

			gray[idx] = (0.2126*float64(c.R) + 0.7152*float64(c.G) + 0.0722*float64(c.B)) / 255.0
		}
	}

	return &ColorData{width, height, gray, r, g, b}, nil
}

// ==================== 图像缩放 ====================

func resizeImageBilinear(src *ColorData, newWidth, newHeight int) *ColorData {
	if newWidth <= 1 || newHeight <= 1 {
		return src
	}
	if newWidth == src.Width && newHeight == src.Height {
		return src
	}

	total := newWidth * newHeight
	dstGray := make([]float64, total)
	dstR := make([]uint8, total)
	dstG := make([]uint8, total)
	dstB := make([]uint8, total)

	scaleX := float64(src.Width-1) / float64(newWidth-1)
	scaleY := float64(src.Height-1) / float64(newHeight-1)

	for y := 0; y < newHeight; y++ {
		for x := 0; x < newWidth; x++ {
			srcX := float64(x) * scaleX
			srcY := float64(y) * scaleY

			x0 := int(srcX)
			y0 := int(srcY)
			x1 := x0 + 1
			y1 := y0 + 1

			if x1 >= src.Width {
				x1 = src.Width - 1
			}
			if y1 >= src.Height {
				y1 = src.Height - 1
			}

			fx := srcX - float64(x0)
			fy := srcY - float64(y0)

			idx00 := y0*src.Width + x0
			idx01 := y0*src.Width + x1
			idx10 := y1*src.Width + x0
			idx11 := y1*src.Width + x1

			w00 := (1 - fx) * (1 - fy)
			w01 := fx * (1 - fy)
			w10 := (1 - fx) * fy
			w11 := fx * fy

			dstIdx := y*newWidth + x

			dstGray[dstIdx] = src.Gray[idx00]*w00 + src.Gray[idx01]*w01 +
				src.Gray[idx10]*w10 + src.Gray[idx11]*w11

			dstR[dstIdx] = uint8(math.Round(
				float64(src.R[idx00])*w00 + float64(src.R[idx01])*w01 +
					float64(src.R[idx10])*w10 + float64(src.R[idx11])*w11))
			dstG[dstIdx] = uint8(math.Round(
				float64(src.G[idx00])*w00 + float64(src.G[idx01])*w01 +
					float64(src.G[idx10])*w10 + float64(src.G[idx11])*w11))
			dstB[dstIdx] = uint8(math.Round(
				float64(src.B[idx00])*w00 + float64(src.B[idx01])*w01 +
					float64(src.B[idx10])*w10 + float64(src.B[idx11])*w11))
		}
	}

	return &ColorData{newWidth, newHeight, dstGray, dstR, dstG, dstB}
}

// ==================== 高斯模糊 ====================

func gaussianBlurGray(gray []float64, width, height int, sigma float64) []float64 {
	kernelSize := int(math.Ceil(sigma * 3))
	if kernelSize%2 == 0 {
		kernelSize++
	}
	if kernelSize < 3 {
		kernelSize = 3
	}

	kernel := make([]float64, kernelSize)
	sum := 0.0
	center := kernelSize / 2

	for i := 0; i < kernelSize; i++ {
		x := float64(i - center)
		kernel[i] = math.Exp(-(x * x) / (2 * sigma * sigma))
		sum += kernel[i]
	}
	for i := range kernel {
		kernel[i] /= sum
	}

	result := make([]float64, width*height)
	temp := make([]float64, width*height)

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var s float64
			for kx := -center; kx <= center; kx++ {
				px := x + kx
				if px < 0 {
					px = 0
				} else if px >= width {
					px = width - 1
				}
				s += gray[y*width+px] * kernel[kx+center]
			}
			temp[y*width+x] = s
		}
	}

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var s float64
			for ky := -center; ky <= center; ky++ {
				py := y + ky
				if py < 0 {
					py = 0
				} else if py >= height {
					py = height - 1
				}
				s += temp[py*width+x] * kernel[ky+center]
			}
			result[y*width+x] = s
		}
	}

	return result
}

// ==================== 边缘检测 ====================

func differenceOfGaussians(gray []float64, width, height int, sigma1, sigma2, threshold float64) []float64 {
	blur1 := gaussianBlurGray(gray, width, height, sigma1)
	blur2 := gaussianBlurGray(gray, width, height, sigma2)

	edges := make([]float64, width*height)
	for i := range edges {
		diff := blur1[i] - blur2[i]
		if diff >= threshold {
			edges[i] = 1.0
		}
	}
	return edges
}

func sobelGradient(gray []float64, width, height int) (gx, gy []float64) {
	gx = make([]float64, width*height)
	gy = make([]float64, width*height)

	for y := 1; y < height-1; y++ {
		for x := 1; x < width-1; x++ {
			tl := gray[(y-1)*width+(x-1)]
			tm := gray[(y-1)*width+x]
			tr := gray[(y-1)*width+(x+1)]
			ml := gray[y*width+(x-1)]
			mr := gray[y*width+(x+1)]
			bl := gray[(y+1)*width+(x-1)]
			bm := gray[(y+1)*width+x]
			br := gray[(y+1)*width+(x+1)]

			gx[y*width+x] = -tl - 2*ml - bl + tr + 2*mr + br
			gy[y*width+x] = -tl - 2*tm - tr + bl + 2*bm + br
		}
	}
	return
}

// ==================== ASCII渲染 ====================

type Pixel struct {
	Char          rune
	R, G, B       uint8 // 前景色
	BgR, BgG, BgB uint8 // 背景色
}

type ASCIIRenderer struct {
	width, height int
	pixels        []Pixel
}

func NewASCIIRenderer(width, height int) *ASCIIRenderer {
	return &ASCIIRenderer{
		width:  width,
		height: height,
		pixels: make([]Pixel, width*height),
	}
}

func (r *ASCIIRenderer) Render(imgData *ColorData, exposure, attenuation float64) {
	edges := differenceOfGaussians(imgData.Gray, imgData.Width, imgData.Height, 0.8, 1.6, 0.08)

	for y := 0; y < r.height; y++ {
		for x := 0; x < r.width && x < imgData.Width; x++ {
			// 钳位：确保不越界
			imgY1 := y * 2
			if imgY1 >= imgData.Height {
				imgY1 = imgData.Height - 1
			}
			imgY2 := imgY1 + 1
			if imgY2 >= imgData.Height {
				imgY2 = imgData.Height - 1
			}

			idx1 := imgY1*imgData.Width + x
			idx2 := imgY2*imgData.Width + x

			lum1 := imgData.Gray[idx1]
			r1, g1, b1 := imgData.R[idx1], imgData.G[idx1], imgData.B[idx1]
			edge1 := edges[idx1] > 0.5

			lum2 := imgData.Gray[idx2]
			r2, g2, b2 := imgData.R[idx2], imgData.G[idx2], imgData.B[idx2]
			edge2 := edges[idx2] > 0.5

			adjLum1 := math.Pow(lum1*exposure, attenuation)
			adjLum2 := math.Pow(lum2*exposure, attenuation)

			var char rune
			var fgR, fgG, fgB uint8
			var bgR, bgG, bgB uint8

			if !edge1 && !edge2 {
				if adjLum1 >= adjLum2 {
					char = '▀'
					fgR, fgG, fgB = r1, g1, b1
					bgR, bgG, bgB = r2, g2, b2
				} else {
					char = '▄'
					fgR, fgG, fgB = r2, g2, b2
					bgR, bgG, bgB = r1, g1, b1
				}
			} else {
				char = '█'
				fgR = uint8((int(r1) + int(r2)) / 2)
				fgG = uint8((int(g1) + int(g2)) / 2)
				fgB = uint8((int(b1) + int(b2)) / 2)
				bgR, bgG, bgB = 0, 0, 0
			}

			outIdx := y*r.width + x
			r.pixels[outIdx] = Pixel{
				Char: char,
				R:    fgR, G: fgG, B: fgB,
				BgR: bgR, BgG: bgG, BgB: bgB,
			}
		}
	}
}

func (r *ASCIIRenderer) String() string {
	var builder strings.Builder
	builder.Grow(r.width*r.height*30 + r.height)

	prevFgR, prevFgG, prevFgB := uint8(255), uint8(255), uint8(255)
	prevBgR, prevBgG, prevBgB := uint8(0), uint8(0), uint8(0)
	first := true

	for y := 0; y < r.height; y++ {
		for x := 0; x < r.width; x++ {
			p := r.pixels[y*r.width+x]

			fgChanged := first || p.R != prevFgR || p.G != prevFgG || p.B != prevFgB
			bgChanged := first || p.BgR != prevBgR || p.BgG != prevBgG || p.BgB != prevBgB

			if fgChanged && bgChanged {
				builder.WriteString(colorFgBg(p.R, p.G, p.B, p.BgR, p.BgG, p.BgB))
			} else if fgChanged {
				builder.WriteString(colorFg(p.R, p.G, p.B))
			} else if bgChanged {
				builder.WriteString(colorBg(p.BgR, p.BgG, p.BgB))
			}

			prevFgR, prevFgG, prevFgB = p.R, p.G, p.B
			prevBgR, prevBgG, prevBgB = p.BgR, p.BgG, p.BgB
			first = false

			builder.WriteRune(p.Char)
		}
		if y < r.height-1 {
			builder.WriteByte('\n')
		}
	}

	builder.WriteString(RESET_COLORS)
	return builder.String()
}

func (r *ASCIIRenderer) Print() {
	fmt.Print(r.String())
}

// ==================== 工具函数 ====================

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// ==================== 自适应缩放 ====================
// 每个终端格子 = 1列 × 2行像素（半块字符）
// 所以把终端高度视为 termHeight*2 来计算等价缩放比

func calculateOutputSize(imgWidth, imgHeight, termWidth, termHeight int) (int, int) {
	imgAspect := float64(imgWidth) / float64(imgHeight)

	// 有效终端高度（像素行）= 字符行数 × 2
	effectiveHeight := termHeight * 2
	cellAspect := float64(termWidth) / float64(effectiveHeight)

	var outWidth, outHeight int

	if imgAspect > cellAspect {
		// 图像更宽，以终端宽度为准
		outWidth = termWidth - 2
		outHeight = int(math.Round(float64(outWidth) / imgAspect))
	} else {
		// 图像更高，以终端高度为准
		outHeight = effectiveHeight - 4
		outWidth = int(math.Round(float64(outHeight) * imgAspect))
	}

	// 确保高度为偶数（每个字符行 = 2像素行）
	if outHeight%2 != 0 {
		outHeight--
	}

	// 二次约束：确保渲染行数不超过终端
	rendererHeight := outHeight / 2
	if rendererHeight > termHeight-2 {
		rendererHeight = termHeight - 2
		outHeight = rendererHeight * 2
		outWidth = int(math.Round(float64(outHeight) * imgAspect))
	}

	if outWidth > termWidth-2 {
		outWidth = termWidth - 2
	}
	if outWidth < 10 {
		outWidth = 10
	}
	if outHeight < 10 {
		outHeight = 10
	}

	return outWidth, outHeight
}

// ==================== 主函数 ====================

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: go run main.go <图片文件.jpg|png>")
		fmt.Println("示例: go run main.go photo.jpg")
		os.Exit(1)
	}

	filename := os.Args[1]

	termSize, err := getTerminalSize()
	if err != nil {
		fmt.Printf("获取终端尺寸失败: %v，使用默认 80x24\n", err)
		termSize = &TerminalSize{Width: 80, Height: 24}
	}

	fmt.Printf("终端尺寸: %dx%d\n", termSize.Width, termSize.Height)

	fmt.Printf("加载图像: %s\n", filename)
	imgData, err := loadImage(filename)
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("原始尺寸: %dx%d\n", imgData.Width, imgData.Height)

	outWidth, outHeight := calculateOutputSize(
		imgData.Width, imgData.Height,
		termSize.Width, termSize.Height,
	)
	fmt.Printf("输出像素: %dx%d\n", outWidth, outHeight)

	fmt.Println("缩放图像...")
	scaledData := resizeImageBilinear(imgData, outWidth, outHeight)

	// 渲染器高度 = 像素行数 / 2（每个字符行编码2个像素行）
	rendererHeight := outHeight / 2
	fmt.Printf("渲染尺寸: %dx%d 字符 (%d 像素行)\n", outWidth, rendererHeight, outHeight)

	fmt.Println("渲染彩色ASCII艺术...")
	renderer := NewASCIIRenderer(outWidth, rendererHeight)
	renderer.Render(scaledData, 1.0, 0.85)

	clearScreen()
	hideCursor()

	renderer.Print()

	fmt.Printf("\033[%d;1H\033[90m[ 按回车键退出 ]%s", termSize.Height, RESET_COLORS)
	fmt.Scanln()

	showCursor()
	clearScreen()
}
