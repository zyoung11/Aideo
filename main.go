package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// ==================== 全局模式选择 ====================
// 优先级：命令行参数 -sixel > 环境变量 AIDEO_SIXEL > Braille 默认

var useSixel bool

// ==================== 主函数 ====================

func init() {
	// 环境变量支持：AIDEO_SIXEL=1 启用 Sixel 渲染
	if os.Getenv("AIDEO_SIXEL") == "1" {
		useSixel = true
	}
}

func main() {
	// 顶层恢复，确保终端状态恢复
	defer func() {
		if r := recover(); r != nil {
			fmt.Print(ENABLE_MOUSE)
			fmt.Print(SHOW_CURSOR)
			fmt.Print(LEAVE_ALTERNATE)
			fmt.Fprintf(os.Stderr, "程序发生 panic: %v\n", r)
			os.Exit(1)
		}
	}()

	if len(os.Args) < 2 {
		fmt.Println("用法: go run main.go [选项] <图片/视频文件>")
		fmt.Println("选项:")
		fmt.Println("  -sixel       使用 Sixel 格式渲染（默认使用 Braille）")
		fmt.Println("支持格式: .jpg .jpeg .png .mp4 .mov .mkv")
		fmt.Println("示例: go run main.go photo.jpg")
		fmt.Println("示例: go run main.go -sixel photo.jpg")
		fmt.Println("示例: go run main.go video.mp4")
		fmt.Println("环境变量: AIDEO_SIXEL=1 等同于 -sixel")
		os.Exit(1)
	}

	// 解析参数
	filename := ""
	sixelFlag := false
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if arg == "-sixel" {
			sixelFlag = true
		} else if filename == "" {
			filename = arg
		}
	}

	if sixelFlag {
		useSixel = true
	}

	// 如果是视频文件，直接播放视频
	if isVideoFile(filename) {
		initVideoPlayback(filename)
		return
	}

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
		brailleRenderer     *BrailleRenderer
		sixelRenderer       *SixelRenderer
	)

	// 按模式初始化渲染
	if useSixel {
		// Sixel 模式：只创建图像尺寸的画布，然后定位光标居中
		outWidth, outHeight = calculateSixelOutputSize(
			imgData.Width, imgData.Height,
			termSize.Width, termSize.Height,
		)
		fmt.Printf("图像渲染: %dx%d 像素\n", outWidth, outHeight)

		scaledData = resizeImageBilinear(imgData, outWidth, outHeight)

		// 创建与图像尺寸相同的画布（不需要全屏画布）
		sixelRenderer = NewSixelRenderer(outWidth, outHeight)
		sixelRenderer.Render(scaledData, 1.0, 0.85)

		// 计算字符尺寸用于光标定位
		charW = (outWidth + defaultCellPixelW - 1) / defaultCellPixelW
		charH = (outHeight + defaultCellPixelH - 1) / defaultCellPixelH
		fmt.Printf("Sixel 图像: %dx%d 像素, 约 %dx%d 字符\n", outWidth, outHeight, charW, charH)
	} else {
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

		brailleRenderer = NewBrailleRenderer(charW, charH)
		brailleRenderer.Render(scaledData, 1.0, 0.85)
	}

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

	// 清屏
	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)

	// 渲染图像
	var imageStr string
	if useSixel {
		// Sixel 模式：计算居中位置并输出
		startCol := (termSize.Width - charW) / 2
		startRow := (termSize.Height - charH) / 2
		if startCol < 1 {
			startCol = 1
		}
		if startRow < 1 {
			startRow = 1
		}
		// 清屏并定位光标到居中位置
		fmt.Print(CLEAR_SCREEN)
		fmt.Printf("\x1b[%d;%dH", startRow, startCol)
		imageStr = sixelRenderer.String()
		fmt.Print(imageStr)
	} else {
		// Braille 模式：计算居中位置
		startCol := (termSize.Width - charW) / 2
		startRow := (termSize.Height - charH) / 2
		if startCol < 0 {
			startCol = 0
		}
		if startRow < 0 {
			startRow = 0
		}

		imageStr = brailleRenderer.String()
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

				if useSixel {
					canvasW, canvasH := getTerminalPixelSize(termSize.Width, termSize.Height)
					outWidth, outHeight = calculateSixelOutputSize(
						imgData.Width, imgData.Height,
						termSize.Width, termSize.Height,
					)
					scaledData = resizeImageBilinear(imgData, outWidth, outHeight)
					sixelRenderer = NewSixelRenderer(canvasW, canvasH)
					sixelRenderer.setImagePlacement(outWidth, outHeight)
					sixelRenderer.Render(scaledData, 1.0, 0.85)
					charW = canvasW
					charH = canvasH
				} else {
					outWidth, outHeight = calculateOutputSize(
						imgData.Width, imgData.Height,
						termSize.Width, termSize.Height,
					)
					scaledData = resizeImageBilinear(imgData, outWidth, outHeight)
					charW = outWidth / 2
					charH = outHeight / 4
					brailleRenderer = NewBrailleRenderer(charW, charH)
					brailleRenderer.Render(scaledData, 1.0, 0.85)
				}

				// 清屏 + 定位光标到左上角 (1,1)
				fmt.Print(CLEAR_SCREEN + "\x1b[1;1H")

				// 渲染图像
				if useSixel {
					// Sixel 全屏画布，直接输出（光标已在 1;1）
					fmt.Print(sixelRenderer.String())
				} else {
					// Braille 模式：计算居中位置
					newStartCol := (termSize.Width - charW) / 2
					newStartRow := (termSize.Height - charH) / 2
					if newStartCol < 0 {
						newStartCol = 0
					}
					if newStartRow < 0 {
						newStartRow = 0
					}

					imageStr := brailleRenderer.String()
					lines := strings.Split(imageStr, "\n")

					for i, line := range lines {
						if line == "" {
							continue
						}
						fmt.Printf("\033[%d;%dH%s", newStartRow+i+1, newStartCol+1, line)
					}

					// 清除图片右侧的空白区域
					if charW > 0 && newStartCol+charW <= termSize.Width {
						clearStartCol := newStartCol + charW + 1
						for row := newStartRow + 1; row <= newStartRow+charH; row++ {
							fmt.Printf("\033[%d;%dH\033[K", row, clearStartCol)
						}
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
