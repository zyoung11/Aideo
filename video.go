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
	"sync"
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

// ==================== 视频流解码 ====================

// decodeVideo 探测视频信息
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
			// raw[offset+3] 是 alpha，忽略

			r[idx] = pr
			g[idx] = pg
			b[idx] = pb

			gray[idx] = (0.2126*float64(pr) + 0.7152*float64(pg) + 0.0722*float64(pb)) / 255.0
		}
	}

	return &ColorData{width, height, gray, r, g, b}
}

// ==================== 视频播放渲染 ====================

type VideoPlayer struct {
	videoInfo   *VideoInfo
	frameReader io.ReadCloser
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

	renderer  *BrailleRenderer
	buf       []byte
	frameSize int

	// 统计
	frameCount int
}

func NewVideoPlayer(videoInfo *VideoInfo, frameReader io.ReadCloser, termWidth, termHeight int) *VideoPlayer {
	charW := videoInfo.Width / 2
	charH := videoInfo.Height / 4

	startCol := (termWidth - charW) / 2
	startRow := (termHeight - charH) / 2
	if startCol < 0 {
		startCol = 0
	}
	if startRow < 0 {
		startRow = 0
	}

	fps := videoInfo.FPS
	if fps <= 0 {
		fps = 30
	}
	// 限制最大帧率
	if fps > 60 {
		fps = 60
	}

	return &VideoPlayer{
		videoInfo:   videoInfo,
		frameReader: frameReader,
		charW:       charW,
		charH:       charH,
		quit:        make(chan struct{}),
		done:        make(chan struct{}),
		fps:         fps,
		frameTime:   time.Duration(float64(time.Second) / fps),
		exposure:    1.0,
		attenuation: 0.85,
		termWidth:   termWidth,
		termHeight:  termHeight,
		startRow:    startRow,
		startCol:    startCol,
		renderer:    NewBrailleRenderer(charW, charH),
		buf:         make([]byte, videoInfo.Width*videoInfo.Height*4),
		frameSize:   videoInfo.Width * videoInfo.Height * 4,
	}
}

// Play 同步播放视频
func (vp *VideoPlayer) Play() {
	defer close(vp.done)

	// 信号监听
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	// fps 计时器
	fpsTicker := time.NewTicker(vp.frameTime)
	defer fpsTicker.Stop()

	// 键盘输入 goroutine（使用 raw mode 读取单个按键）
	keyCh := make(chan byte, 10)
	go func() {
		defer func() { recover() }()

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
			// ESC 检测
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
					// 超时 → 单独的 ESC 键
				}
			}

			select {
			case keyCh <- b:
			default:
			}
		}
	}()

	// 渲染缓冲区
	var outputBuf strings.Builder
	outputBuf.Grow(vp.charW*vp.charH*30 + vp.charH)

	vp.frameCount = 0

	// 主循环
	for {
		select {
		case <-vp.quit:
			return
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP:
				return
			case syscall.SIGWINCH:
				// 窗口大小变化
				newSize, err := getTerminalSize()
				if err != nil {
					continue
				}
				charW := vp.videoInfo.Width / 2
				charH := vp.videoInfo.Height / 4
				newStartCol := (newSize.Width - charW) / 2
				newStartRow := (newSize.Height - charH) / 2
				if newStartCol < 0 {
					newStartCol = 0
				}
				if newStartRow < 0 {
					newStartRow = 0
				}
				vp.termWidth = newSize.Width
				vp.termHeight = newSize.Height
				vp.startCol = newStartCol
				vp.startRow = newStartRow
			}
		case key, ok := <-keyCh:
			if !ok {
				return
			}
			if key == 'q' || key == 'Q' || key == 27 {
				return
			}
		default:
		}

		// 读取一帧 RGBA 数据
		n, err := io.ReadFull(vp.frameReader, vp.buf)
		if err != nil || n != vp.frameSize {
			return // 视频结束
		}

		// 转换 raw RGBA → ColorData
		imgData := rawRGBAToColorData(vp.buf, vp.videoInfo.Width, vp.videoInfo.Height)

		// 渲染
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

		vp.frameCount++

		// 等待下一个帧时间
		<-fpsTicker.C
	}
}

// Stop 停止播放
func (vp *VideoPlayer) Stop() {
	select {
	case vp.quit <- struct{}{}:
	default:
	}
}

// Wait 等待播放结束
func (vp *VideoPlayer) Wait() {
	<-vp.done
}

// ==================== 播放入口 ====================

func playVideo(filename string) error {
	// 获取终端尺寸
	termSize, err := getTerminalSize()
	if err != nil {
		fmt.Printf("获取终端尺寸失败: %v，使用默认 80x24\n", err)
		termSize = &TerminalSize{Width: 80, Height: 24}
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

	// 显示加载信息
	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)
	fmt.Printf("\033[%d;%dH\033[90m正在探测视频...%s",
		termSize.Height/2, termSize.Width/2-10, RESET_COLORS)

	// 探测视频信息
	info, err := probeVideo(filename)
	if err != nil {
		return fmt.Errorf("视频探测失败: %v", err)
	}

	// 清屏
	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)

	// 计算输出尺寸
	outWidth, outHeight := calculateOutputSize(
		info.Width, info.Height,
		termSize.Width, termSize.Height,
	)
	info.Width = outWidth
	info.Height = outHeight

	// 启动 FFmpeg 解码进程
	ffmpegPath := getFFmpegPath()
	pr, pw := io.Pipe()

	stream := ffmpeg.Input(filename).
		Output("pipe:",
			ffmpeg.KwArgs{
				"format":   "rawvideo",
				"pix_fmt":  "rgba",
				"s":        fmt.Sprintf("%dx%d", info.Width, info.Height),
				"an":       "",
				"sn":       "",
			}).
		Silent(true).
		SetFfmpegPath(ffmpegPath).
		WithOutput(pw)

	go func() {
		err := stream.Run()
		if err != nil {
			pw.CloseWithError(fmt.Errorf("ffmpeg 错误: %v", err))
		} else {
			pw.Close()
		}
	}()

	// 设置键盘 raw 模式
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		pr.Close()
		return fmt.Errorf("无法设置 raw 模式: %v", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	player := NewVideoPlayer(info, pr, termSize.Width, termSize.Height)

	// 窗口大小变化监听
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	var mu sync.Mutex
	go func() {
		for range sigCh {
			mu.Lock()
			newSize, err := getTerminalSize()
			if err != nil {
				mu.Unlock()
				continue
			}
			charW := info.Width / 2
			charH := info.Height / 4
			newStartCol := (newSize.Width - charW) / 2
			newStartRow := (newSize.Height - charH) / 2
			if newStartCol < 0 {
				newStartCol = 0
			}
			if newStartRow < 0 {
				newStartRow = 0
			}
			player.termWidth = newSize.Width
			player.termHeight = newSize.Height
			player.startCol = newStartCol
			player.startRow = newStartRow
			mu.Unlock()
		}
	}()

	player.Play()

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
