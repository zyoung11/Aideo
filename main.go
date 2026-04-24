package main

import (
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/signal"
	"strings"
	"syscall"

	timage "Aideo/image"

	"golang.org/x/term"
)

// ==================== 主函数 ====================

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
		fmt.Println("用法: go run main.go <图片/视频文件>")
		fmt.Println("支持格式: .jpg .jpeg .png .mp4 .mov .mkv")
		fmt.Println("示例: go run main.go photo.jpg")
		fmt.Println("示例: go run main.go video.mp4")
		os.Exit(1)
	}

	filename := os.Args[1]

	// 如果是视频文件，直接播放视频
	if isVideoFile(filename) {
		initVideoPlayback(filename)
		return
	}

	protocol, hasProtocol := timage.DetectCapableProtocol()
	if hasProtocol {
		img, err := loadImageAsRGBA(filename)
		if err != nil {
			fmt.Printf("错误: %v\n", err)
			os.Exit(1)
		}
		cfg := timage.DefaultConfig()
		cfg.Img = img
		cfg.ForceProtocol = protocol
		_, err = timage.ShowImage(cfg)
		timage.ClearImage(protocol)
		if err != nil {
			fmt.Printf("错误: %v\n", err)
			os.Exit(1)
		}
		return
	}

	termSize, err := getTerminalSize()
	if err != nil {
		fmt.Printf("获取终端尺寸失败: %v，使用默认 80x24\n", err)
		termSize = &TerminalSize{Width: 80, Height: 24}
	}

	imgData, err := loadImage(filename)
	if err != nil {
		fmt.Printf("错误: %v\n", err)
		os.Exit(1)
	}

	var (
		outWidth, outHeight int
		scaledData          *ColorData
		charW, charH        int
		brailleRenderer     *BrailleRenderer
	)

	outWidth, outHeight = calculateOutputSize(
		imgData.Width, imgData.Height,
		termSize.Width, termSize.Height,
	)
	scaledData = resizeImageBilinear(imgData, outWidth, outHeight)

	charW = outWidth / 2
	charH = outHeight / 4

	brailleRenderer = NewBrailleRenderer(charW, charH)
	brailleRenderer.Render(scaledData, 1.0, 0.85)

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
	startCol := (termSize.Width - charW) / 2
	startRow := (termSize.Height - charH) / 2
	if startCol < 0 {
		startCol = 0
	}
	if startRow < 0 {
		startRow = 0
	}

	imageStr := brailleRenderer.String()
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

				outWidth, outHeight = calculateOutputSize(
					imgData.Width, imgData.Height,
					termSize.Width, termSize.Height,
				)
				scaledData = resizeImageBilinear(imgData, outWidth, outHeight)
				charW = outWidth / 2
				charH = outHeight / 4
				brailleRenderer = NewBrailleRenderer(charW, charH)
				brailleRenderer.Render(scaledData, 1.0, 0.85)

				// 清屏 + 定位光标到左上角 (1,1)
				fmt.Print(CLEAR_SCREEN + "\x1b[1;1H")

				// 渲染图像
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

func loadImageAsRGBA(filename string) (*image.RGBA, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("无法打开文件: %v", err)
	}
	defer file.Close()

	img, _, err := image.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("解码图像失败: %v", err)
	}

	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, rgba.Bounds(), img, bounds.Min, draw.Src)
	return rgba, nil
}
