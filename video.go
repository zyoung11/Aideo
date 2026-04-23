package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	ffmpeg "github.com/u2takey/ffmpeg-go"
	"golang.org/x/term"
)

// ==================== 视频元数据 ====================

type VideoInfo struct {
	Width       int
	Height      int
	FPS         float64
	TotalFrames int
	Duration    float64
}

// ==================== FFmpeg 路径检测 ====================

func getFFmpegPath() string {
	if runtime.GOOS == "windows" {
		return "./ffmpeg.exe"
	}
	return "./ffmpeg"
}

// ==================== 视频流探测 ====================

func probeVideo(filename string) (*VideoInfo, error) {
	probeData, err := ffmpeg.Probe(filename)
	if err != nil {
		return nil, fmt.Errorf("无法探测视频信息: %v", err)
	}

	type ProbeStream struct {
		CodecType  string `json:"codec_type"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		AvgFPS     string `json:"avg_frame_rate"`
		RFrameRate string `json:"r_frame_rate"`
		NbFrames   string `json:"nb_frames"`
		Duration   string `json:"duration"`
	}
	type ProbeFormat struct {
		Duration string `json:"duration"`
	}
	type ProbeResult struct {
		Streams []ProbeStream `json:"streams"`
		Format  ProbeFormat   `json:"format"`
	}

	var probe ProbeResult
	if err := json.Unmarshal([]byte(probeData), &probe); err != nil {
		return nil, fmt.Errorf("解析视频信息失败: %v", err)
	}

	var vStream *ProbeStream
	for i := range probe.Streams {
		if probe.Streams[i].CodecType == "video" {
			vStream = &probe.Streams[i]
			break
		}
	}

	if vStream == nil {
		return nil, fmt.Errorf("未找到视频流")
	}

	info := &VideoInfo{
		Width:  vStream.Width,
		Height: vStream.Height,
	}

	// 解析帧率
	if vStream.AvgFPS != "" {
		var num, den int
		n, _ := fmt.Sscanf(vStream.AvgFPS, "%d/%d", &num, &den)
		if n == 2 && den > 0 {
			info.FPS = float64(num) / float64(den)
		} else {
			n2, _ := fmt.Sscanf(vStream.AvgFPS, "%d", &num)
			if n2 == 1 {
				info.FPS = float64(num)
			}
		}
	}
	if info.FPS == 0 && vStream.RFrameRate != "" {
		var num, den int
		n, _ := fmt.Sscanf(vStream.RFrameRate, "%d/%d", &num, &den)
		if n == 2 && den > 0 {
			info.FPS = float64(num) / float64(den)
		} else {
			n2, _ := fmt.Sscanf(vStream.RFrameRate, "%d", &num)
			if n2 == 1 {
				info.FPS = float64(num)
			}
		}
	}
	if info.FPS <= 0 {
		info.FPS = 30
	}

	// 解析总帧数
	if vStream.NbFrames != "" {
		fmt.Sscanf(vStream.NbFrames, "%d", &info.TotalFrames)
	}

	// 解析时长
	if vStream.Duration != "" {
		fmt.Sscanf(vStream.Duration, "%f", &info.Duration)
	}
	if info.Duration == 0 && probe.Format.Duration != "" {
		fmt.Sscanf(probe.Format.Duration, "%f", &info.Duration)
	}

	if info.TotalFrames == 0 && info.Duration > 0 && info.FPS > 0 {
		info.TotalFrames = int(info.Duration * info.FPS)
	}

	return info, nil
}

// ==================== raw RGBA → ColorData ====================

func rawRGBAToColorData(raw []byte, width, height int) *ColorData {
	total := width * height
	gray := make([]float64, total)
	r := make([]uint8, total)
	g := make([]uint8, total)
	b := make([]uint8, total)

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			idx := y*width + x
			offset := idx * 4
			pr := raw[offset]
			pg := raw[offset+1]
			pb := raw[offset+2]

			r[idx] = pr
			g[idx] = pg
			b[idx] = pb

			gray[idx] = (0.2126*float64(pr) + 0.7152*float64(pg) + 0.0722*float64(pb)) / 255.0
		}
	}

	return &ColorData{width, height, gray, r, g, b}
}

// ==================== 视频播放器 ====================

type VideoPlayer struct {
	filename    string
	srcWidth    int
	srcHeight   int
	outWidth    int
	outHeight   int
	charW       int
	charH       int

	quit        chan struct{}
	done        chan struct{}

	fps         float64
	frameTime   time.Duration
	exposure    float64
	attenuation float64

	termWidth   int
	termHeight  int
	startRow    int
	startCol    int

	renderer    *BrailleRenderer
}

func NewVideoPlayer(filename string, srcWidth, srcHeight int, termWidth, termHeight int) *VideoPlayer {
	outWidth, outHeight := calculateOutputSize(srcWidth, srcHeight, termWidth, termHeight)
	charW := outWidth / 2
	charH := outHeight / 4

	startCol := (termWidth - charW) / 2
	startRow := (termHeight - charH) / 2
	if startCol < 0 {
		startCol = 0
	}
	if startRow < 0 {
		startRow = 0
	}

	return &VideoPlayer{
		filename:    filename,
		srcWidth:    srcWidth,
		srcHeight:   srcHeight,
		outWidth:    outWidth,
		outHeight:   outHeight,
		charW:       charW,
		charH:       charH,
		quit:        make(chan struct{}),
		done:        make(chan struct{}),
		fps:         30,
		frameTime:   time.Second / 30,
		exposure:    1.0,
		attenuation: 0.85,
		termWidth:   termWidth,
		termHeight:  termHeight,
		startRow:    startRow,
		startCol:    startCol,
		renderer:    NewBrailleRenderer(charW, charH),
	}
}

// updateTerminalSize 窗口变化时重新计算输出尺寸和居中位置
func (vp *VideoPlayer) updateTerminalSize(newWidth, newHeight int) {
	vp.termWidth = newWidth
	vp.termHeight = newHeight

	newOutW, newOutH := calculateOutputSize(
		vp.srcWidth, vp.srcHeight,
		newWidth, newHeight,
	)

	vp.outWidth = newOutW
	vp.outHeight = newOutH
	vp.charW = newOutW / 2
	vp.charH = newOutH / 4
	vp.renderer = NewBrailleRenderer(vp.charW, vp.charH)

	newStartCol := (newWidth - vp.charW) / 2
	newStartRow := (newHeight - vp.charH) / 2
	if newStartCol < 0 {
		newStartCol = 0
	}
	if newStartRow < 0 {
		newStartRow = 0
	}
	vp.startCol = newStartCol
	vp.startRow = newStartRow
}

// ==================== 双缓冲解码器 ====================

type decoder struct {
	reader  io.ReadCloser
	closeCh chan struct{} // 通知 decoder goroutine 退出
	doneCh  chan struct{} // decoder goroutine 已退出
}

// spawnDecoder 启动一个 ffmpeg 解码进程
func (vp *VideoPlayer) spawnDecoder() *decoder {
	ffmpegPath := getFFmpegPath()
	pr, pw := io.Pipe()

	stream := ffmpeg.Input(vp.filename).
		Output("pipe:",
			ffmpeg.KwArgs{
				"format":   "rawvideo",
				"pix_fmt":  "rgba",
				"s":        fmt.Sprintf("%dx%d", vp.outWidth, vp.outHeight),
				"an":       "",
				"sn":       "",
			}).
		Silent(true).
		SetFfmpegPath(ffmpegPath).
		WithOutput(pw)

	dec := &decoder{
		reader:  pr,
		closeCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	go func() {
		defer close(dec.doneCh)
		// 启动 ffmpeg
		errCh := make(chan error, 1)
		go func() {
			errCh <- stream.Run()
		}()

		select {
		case <-dec.closeCh:
			// 被外部关闭：终止 ffmpeg（关闭 pipe 读取端使 ffmpeg 收到 SIGPIPE）
			pr.Close()
			// 等待 ffmpeg 退出（忽略错误）
			<-errCh
		case err := <-errCh:
			if err != nil {
				pw.CloseWithError(fmt.Errorf("ffmpeg 错误: %v", err))
			} else {
				pw.Close()
			}
		}
	}()

	return dec
}

// close 关闭 decoder
func (d *decoder) close() {
	close(d.closeCh)
	<-d.doneCh
}

// ==================== 播放循环 ====================

// startLoop 循环播放，使用双缓冲 decoder 实现无缝循环
func (vp *VideoPlayer) startLoop() {
	defer close(vp.done)

	// 信号监听
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	// 键盘输入 goroutine
	keyCh := make(chan byte, 10)
	keyDone := make(chan struct{})
	go func() {
		defer func() { recover() }()
		defer close(keyDone)

		oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)

		reader := bufio.NewReader(os.Stdin)
		for {
			b, err := reader.ReadByte()
			if err != nil {
				close(keyCh)
				return
			}
			if b == 27 {
				type readResult struct {
					b   byte
					err error
				}
				ch := make(chan readResult, 1)
				go func() {
					nb, ne := reader.ReadByte()
					ch <- readResult{nb, ne}
				}()

				select {
				case r := <-ch:
					if r.err == nil {
						next := r.b
						if next == '[' || next == 'O' || next == 'M' {
							for {
								rb, re := reader.ReadByte()
								if re != nil {
									break
								}
								if (rb >= 'A' && rb <= 'Z') || (rb >= 'a' && rb <= 'z') || rb == '~' {
									break
								}
							}
							continue
						}
					}
				case <-time.After(50 * time.Millisecond):
				}
			}

			select {
			case keyCh <- b:
			default:
			}
		}
	}()

	frameSize := vp.outWidth * vp.outHeight * 4
	var outputBuf strings.Builder
	outputBuf.Grow(vp.charW*vp.charH*30 + vp.charH)

	// 启动第一个解码器
	currentDec := vp.spawnDecoder()
	// 预启动第二个解码器（双缓冲）
	nextDec := vp.spawnDecoder()

	// 清屏一次（只在最开始时）
	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)

	fpsTicker := time.NewTicker(vp.frameTime)
	defer fpsTicker.Stop()

	// 主帧循环
	for {
		select {
		case <-vp.quit:
			currentDec.close()
			nextDec.close()
			return
		case <-sigCh:
			// 窗口变化：丢弃所有解码器，重新开始
			currentDec.close()
			nextDec.close()
			vp.updateTerminalSizeFromSigwinch()
			// 重新计算 frameSize
			frameSize = vp.outWidth * vp.outHeight * 4
			outputBuf.Grow(vp.charW*vp.charH*30 + vp.charH)
			currentDec = vp.spawnDecoder()
			nextDec = vp.spawnDecoder()
			// 清屏消除残留
			fmt.Print(CLEAR_SCREEN + CURSOR_HOME)
			continue
		case key, ok := <-keyCh:
			if !ok {
				currentDec.close()
				nextDec.close()
				return
			}
			if key == 'q' || key == 'Q' || key == 27 {
				currentDec.close()
				nextDec.close()
				return
			}
		default:
		}

		// 读取一帧
		buf := make([]byte, frameSize)
		n, err := io.ReadFull(currentDec.reader, buf)
		if err != nil || n != frameSize {
			// 当前视频结束 → 无缝切换到下一个 decoder
			currentDec.close()

			// 交换：nextDec 变成 currentDec
			currentDec = nextDec
			// 启动新的 nextDec
			nextDec = vp.spawnDecoder()

			// 继续读（不执行任何清屏操作）
			continue
		}

		// 渲染
		imgData := rawRGBAToColorData(buf, vp.outWidth, vp.outHeight)
		vp.renderer.Render(imgData, vp.exposure, vp.attenuation)

		// 构建输出
		outputBuf.Reset()
		imageStr := vp.renderer.String()
		lines := strings.Split(imageStr, "\n")

		for i, line := range lines {
			if line == "" {
				continue
			}
			outputBuf.WriteString(fmt.Sprintf("\033[%d;%dH%s",
				vp.startRow+i+1, vp.startCol+1, line))
		}

		// 清除右侧空白
		if vp.charW > 0 && vp.startCol+vp.charW <= vp.termWidth {
			clearStartCol := vp.startCol + vp.charW + 1
			for row := vp.startRow + 1; row <= vp.startRow+vp.charH; row++ {
				outputBuf.WriteString(fmt.Sprintf("\033[%d;%dH\033[K", row, clearStartCol))
			}
		}

		// 底部提示
		outputBuf.WriteString(fmt.Sprintf("\033[%d;1H\033[90m[ 按 q 退出 ]\033[K%s",
			vp.termHeight, RESET_COLORS))

		fmt.Print(outputBuf.String())

		// 等待下一个帧时间
		<-fpsTicker.C
	}
}

// updateTerminalSizeFromSigwinch 从系统获取当前终端尺寸并更新
func (vp *VideoPlayer) updateTerminalSizeFromSigwinch() {
	newSize, err := getTerminalSize()
	if err != nil {
		return
	}
	vp.updateTerminalSize(newSize.Width, newSize.Height)
}

// ==================== 播放入口 ====================

func playVideo(filename string) error {
	// 获取终端尺寸
	termSize, err := getTerminalSize()
	if err != nil {
		return fmt.Errorf("获取终端尺寸失败: %v", err)
	}

	// 进入 alternate screen
	fmt.Print(ENTER_ALTERNATE)
	fmt.Print(HIDE_CURSOR)
	fmt.Print(DISABLE_MOUSE)
	defer func() {
		fmt.Print(ENABLE_MOUSE)
		fmt.Print(SHOW_CURSOR)
		fmt.Print(LEAVE_ALTERNATE)
	}()

	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)
	fmt.Printf("\033[%d;%dH\033[90m正在探测视频...%s",
		termSize.Height/2, termSize.Width/2-10, RESET_COLORS)

	// 探测视频信息
	info, err := probeVideo(filename)
	if err != nil {
		return fmt.Errorf("视频探测失败: %v", err)
	}

	// 创建播放器
	player := NewVideoPlayer(filename, info.Width, info.Height, termSize.Width, termSize.Height)
	player.fps = info.FPS
	player.frameTime = time.Duration(float64(time.Second) / info.FPS)

	// 设置键盘 raw 模式
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("无法设置 raw 模式: %v", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	player.startLoop()

	return nil
}

// ==================== 判断是否为视频文件 ====================

func isVideoFile(filename string) bool {
	lower := strings.ToLower(filename)
	return strings.HasSuffix(lower, ".mp4") ||
		strings.HasSuffix(lower, ".mov") ||
		strings.HasSuffix(lower, ".mkv")
}

// ==================== 视频播放入口（供 main.go 调用） ====================

func initVideoPlayback(filename string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Print(ENABLE_MOUSE)
			fmt.Print(SHOW_CURSOR)
			fmt.Print(LEAVE_ALTERNATE)
			fmt.Fprintf(os.Stderr, "视频播放发生 panic: %v\n", r)
			os.Exit(1)
		}
	}()

	err := playVideo(filename)
	if err != nil {
		fmt.Print(ENABLE_MOUSE)
		fmt.Print(SHOW_CURSOR)
		fmt.Print(LEAVE_ALTERNATE)
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}
