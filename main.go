package main

import (
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// ==================== 终端控制 ====================

type TerminalSize struct {
	Width  int
	Height int
}

func getTerminalSize() (*TerminalSize, error) {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return nil, err
	}
	return &TerminalSize{
		Width:  width,
		Height: height,
	}, nil
}

const (
	CLEAR_SCREEN     = "\033[2J"
	CURSOR_HOME      = "\033[H"
	HIDE_CURSOR      = "\033[?25l"
	SHOW_CURSOR      = "\033[?25h"
	RESET_COLORS     = "\033[0m"
	ENTER_ALTERNATE  = "\033[?1049h"
	LEAVE_ALTERNATE  = "\033[?1049l"
	CLEAR_SCROLLBACK = "\033[3J"
	DISABLE_MOUSE    = "\033[?1000l\033[?1002l\033[?1003l"
	ENABLE_MOUSE     = "\033[?1000h\033[?1002h\033[?1003h"
)

func clearScreen()                 { fmt.Print(CLEAR_SCREEN + CURSOR_HOME) }
func hideCursor()                  { fmt.Print(HIDE_CURSOR) }
func showCursor()                  { fmt.Print(SHOW_CURSOR) }
func colorFg(r, g, b uint8) string { return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b) }
func colorBg(r, g, b uint8) string { return fmt.Sprintf("\033[48;2;%d;%d;%dm", r, g, b) }

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

// ==================== Braille 八分块渲染 ====================
//
// 每个 Braille 字符覆盖 2列 × 4行 = 8 个像素点
//
//	Unicode Braille 点位布局:
//	  Dot1(0x01)  Dot4(0x08)    row 0
//	  Dot2(0x02)  Dot5(0x10)    row 1
//	  Dot3(0x04)  Dot6(0x20)    row 2
//	  Dot7(0x40)  Dot8(0x80)    row 3
//
// 基址 U+2800，按位或叠加点位掩码得到字符

var brailleDotMasks = [4][2]uint8{
	{0x01, 0x08},
	{0x02, 0x10},
	{0x04, 0x20},
	{0x40, 0x80},
}

func getBrailleChar(dots [4][2]bool) rune {
	var mask uint8
	for r := 0; r < 4; r++ {
		for c := 0; c < 2; c++ {
			if dots[r][c] {
				mask |= brailleDotMasks[r][c]
			}
		}
	}
	return rune(0x2800 + int(mask))
}

// ==================== Braille 渲染器 ====================

type Pixel struct {
	Char          rune
	R, G, B       uint8 // 前景色（亮点）
	BgR, BgG, BgB uint8 // 背景色（暗点）
}

type BrailleRenderer struct {
	width, height int // 字符网格尺寸
	pixels        []Pixel
}

func NewBrailleRenderer(charWidth, charHeight int) *BrailleRenderer {
	return &BrailleRenderer{
		width:  charWidth,
		height: charHeight,
		pixels: make([]Pixel, charWidth*charHeight),
	}
}

// sortFloat8 对 8 个 float64 做原地排序（冒泡，避免 import sort）
func sortFloat8(a *[8]float64) {
	for i := 0; i < 7; i++ {
		for j := i + 1; j < 8; j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

func (r *BrailleRenderer) Render(imgData *ColorData, exposure, attenuation float64) {
	for cy := 0; cy < r.height; cy++ {
		for cx := 0; cx < r.width; cx++ {
			var dots [4][2]bool
			var lums [8]float64
			var rs [8]uint8
			var gs [8]uint8
			var bs [8]uint8

			// 收集 2×4 像素块
			for dy := 0; dy < 4; dy++ {
				for dx := 0; dx < 2; dx++ {
					px := cx*2 + dx
					py := cy*4 + dy

					if px >= imgData.Width {
						px = imgData.Width - 1
					}
					if py >= imgData.Height {
						py = imgData.Height - 1
					}

					idx := py*imgData.Width + px
					adjLum := math.Pow(imgData.Gray[idx]*exposure, attenuation)

					li := dy*2 + dx
					lums[li] = adjLum
					rs[li] = imgData.R[idx]
					gs[li] = imgData.G[idx]
					bs[li] = imgData.B[idx]
				}
			}

			// 中位数亮度作为分割阈值
			sorted := lums
			sortFloat8(&sorted)
			median := (sorted[3] + sorted[4]) / 2.0

			// 亮 → 前景点，暗 → 背景
			var fgR, fgG, fgB, bgR, bgG, bgB float64
			var fgN, bgN int

			for dy := 0; dy < 4; dy++ {
				for dx := 0; dx < 2; dx++ {
					li := dy*2 + dx
					if lums[li] > median {
						dots[dy][dx] = true
						fgR += float64(rs[li])
						fgG += float64(gs[li])
						fgB += float64(bs[li])
						fgN++
					} else {
						bgR += float64(rs[li])
						bgG += float64(gs[li])
						bgB += float64(bs[li])
						bgN++
					}
				}
			}

			if fgN == 0 {
				fgN = 1
			}
			if bgN == 0 {
				bgN = 1
			}

			char := getBrailleChar(dots)
			// 空白 braille 用普通空格替代，避免某些终端显示异常
			if char == 0x2800 {
				char = ' '
			}

			outIdx := cy*r.width + cx
			r.pixels[outIdx] = Pixel{
				Char: char,
				R:    uint8(fgR / float64(fgN)),
				G:    uint8(fgG / float64(fgN)),
				B:    uint8(fgB / float64(fgN)),
				BgR:  uint8(bgR / float64(bgN)),
				BgG:  uint8(bgG / float64(bgN)),
				BgB:  uint8(bgB / float64(bgN)),
			}
		}
	}
}

func (r *BrailleRenderer) String() string {
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

func (r *BrailleRenderer) Print() {
	fmt.Print(r.String())
}

// 判断是否是转义序列终止符
func isTerminator(b byte) bool {
	// 转义序列终止符通常是字母或 ~
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '~'
}

// ==================== 自适应缩放 ====================
// Braille 模式：每个字符 = 2列 × 4行像素
// 终端字符宽高比通常为 1:2，braille 像素恰好为正方形（cw/2 × ch/4 = cw/2 × cw/2）

func calculateOutputSize(imgWidth, imgHeight, termWidth, termHeight int) (int, int) {
	availCharW := termWidth - 2
	availCharH := termHeight - 2

	// 对应像素分辨率
	availPixelW := availCharW * 2
	availPixelH := availCharH * 4

	imgAspect := float64(imgWidth) / float64(imgHeight)
	cellAspect := float64(availPixelW) / float64(availPixelH)

	var outWidth, outHeight int

	if imgAspect > cellAspect {
		// 图像更宽 → 以宽度为约束
		outWidth = availPixelW
		outHeight = int(math.Round(float64(outWidth) / imgAspect))
	} else {
		// 图像更高 → 以高度为约束
		outHeight = availPixelH
		outWidth = int(math.Round(float64(outHeight) * imgAspect))
	}

	// 像素宽度必须是 2 的倍数，高度必须是 4 的倍数
	outWidth = (outWidth / 2) * 2
	outHeight = (outHeight / 4) * 4

	// 最小尺寸保护
	if outWidth < 4 {
		outWidth = 4
	}
	if outHeight < 8 {
		outHeight = 8
	}

	return outWidth, outHeight
}

// ==================== 主函数 ====================

func main() {
	// 顶层恢复，确保终端状态恢复
	defer func() {
		if r := recover(); r != nil {
			// 恢复终端状态
			fmt.Print(ENABLE_MOUSE)
			fmt.Print(SHOW_CURSOR)
			fmt.Print(LEAVE_ALTERNATE)
			// 打印错误信息
			fmt.Fprintf(os.Stderr, "程序发生 panic: %v\n", r)
			os.Exit(1)
		}
	}()

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

	// 声明变量，以便在窗口大小变化时访问
	var (
		outWidth, outHeight int
		scaledData          *ColorData
		charW, charH        int
		renderer            *BrailleRenderer
	)

	// 初始化渲染
	outWidth, outHeight = calculateOutputSize(
		imgData.Width, imgData.Height,
		termSize.Width, termSize.Height,
	)
	fmt.Printf("输出像素: %dx%d\n", outWidth, outHeight)

	scaledData = resizeImageBilinear(imgData, outWidth, outHeight)

	charW = outWidth / 2
	charH = outHeight / 4
	fmt.Printf("渲染尺寸: %dx%d 字符 (Braille 2×4, 共 %d 像素)\n",
		charW, charH, charW*charH*8)

	renderer = NewBrailleRenderer(charW, charH)
	renderer.Render(scaledData, 1.0, 0.85)

	// 进入 alternate screen 并隐藏光标，禁用鼠标报告
	fmt.Print(ENTER_ALTERNATE)
	fmt.Print(HIDE_CURSOR)
	fmt.Print(DISABLE_MOUSE)
	defer func() {
		// 确保终端状态恢复，即使发生 panic
		fmt.Print(ENABLE_MOUSE)
		fmt.Print(SHOW_CURSOR)
		fmt.Print(LEAVE_ALTERNATE)
	}()

	// 计算居中位置
	startCol := (termSize.Width - charW) / 2
	startRow := (termSize.Height - charH) / 2

	// 确保位置不为负数
	if startCol < 0 {
		startCol = 0
	}
	if startRow < 0 {
		startRow = 0
	}

	// 清屏
	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)

	// 渲染图像到居中位置
	imageStr := renderer.String()
	lines := strings.Split(imageStr, "\n")

	for i, line := range lines {
		if line == "" {
			continue
		}
		// 移动到指定位置并输出该行
		fmt.Printf("\033[%d;%dH%s", startRow+i+1, startCol+1, line)
	}

	// 清除图片右侧的空白区域
	if charW > 0 && startCol+charW <= termSize.Width {
		clearStartCol := startCol + charW + 1
		for row := startRow + 1; row <= startRow+charH; row++ {
			fmt.Printf("\033[%d;%dH\033[K", row, clearStartCol)
		}
	}

	// 在底部显示提示
	fmt.Printf("\033[%d;1H\033[90m[ 按 q 或 ESC 退出 ]%s", termSize.Height, RESET_COLORS)

	// 设置 raw 模式输入
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Printf("无法设置 raw 模式: %v\n", err)
		os.Exit(1)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// 监听信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	// 键盘输入 channel
	keyCh := make(chan byte, 10)
	// 启动 goroutine 读取按键
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// 发生 panic，关闭 channel
				close(keyCh)
			}
		}()

		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				close(keyCh)
				return
			}

			// 处理读取到的字节
			for i := 0; i < n; i++ {
				b := buf[i]

				// 检查是否是转义序列开头
				if b == 27 && i+1 < n {
					// 可能是转义序列，检查下一个字符
					next := buf[i+1]
					// 如果是 '[' 或 'O' 或 'M'，则是转义序列，不是 ESC 键
					// 'M' 是鼠标事件
					if next == '[' || next == 'O' || next == 'M' {
						i++ // 跳过 ESC 后面的字符
						for i < n && !isTerminator(buf[i]) {
							i++
						}
						continue
					}
				}

				// 发送到 channel（非阻塞）
				select {
				case keyCh <- b:
				default:
					// channel 满，丢弃
				}
			}
		}
	}()

	// 事件循环
	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGWINCH:
				// 窗口大小变化，重新获取终端尺寸并重新渲染
				newSize, err := getTerminalSize()
				if err != nil {
					continue
				}
				termSize = newSize
				// 重新计算输出尺寸
				outWidth, outHeight = calculateOutputSize(
					imgData.Width, imgData.Height,
					termSize.Width, termSize.Height,
				)
				// 重新缩放图像
				scaledData = resizeImageBilinear(imgData, outWidth, outHeight)
				charW = outWidth / 2
				charH = outHeight / 4
				// 重新创建渲染器并渲染
				renderer = NewBrailleRenderer(charW, charH)
				renderer.Render(scaledData, 1.0, 0.85)
				// 计算新的居中位置
				newStartCol := (termSize.Width - charW) / 2
				newStartRow := (termSize.Height - charH) / 2
				if newStartCol < 0 {
					newStartCol = 0
				}
				if newStartRow < 0 {
					newStartRow = 0
				}

				// 清屏
				fmt.Print(CLEAR_SCREEN + CURSOR_HOME)

				// 渲染图像到居中位置
				imageStr := renderer.String()
				lines := strings.Split(imageStr, "\n")

				for i, line := range lines {
					if line == "" {
						continue
					}
					// 移动到指定位置并输出该行
					fmt.Printf("\033[%d;%dH%s", newStartRow+i+1, newStartCol+1, line)
				}

				// 清除图片右侧的空白区域
				if charW > 0 && newStartCol+charW <= termSize.Width {
					clearStartCol := newStartCol + charW + 1
					for row := newStartRow + 1; row <= newStartRow+charH; row++ {
						fmt.Printf("\033[%d;%dH\033[K", row, clearStartCol)
					}
				}

				fmt.Printf("\033[%d;1H\033[90m[ 按 q 或 ESC 退出 ]%s", termSize.Height, RESET_COLORS)
			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP:
				return
			}
		case key, ok := <-keyCh:
			if !ok {
				return // channel 关闭
			}
			if key == 'q' || key == 'Q' || key == 27 { // 27 = ESC
				return
			}
			// 忽略其他所有输入
		}
	}
}
