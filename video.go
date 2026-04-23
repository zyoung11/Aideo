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
			// raw[offset+3] 是 alpha，忽略

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
	videoInfo   *VideoInfo

	charW       int
	charH       int
	quit        chan struct{}  // 外部通知停止
	done        chan struct{}  // 播放结束信号

	fps         float64
	frameTime   time.Duration
	exposure    float64
	attenuation float64

	termWidth   int
	termHeight  int
	startRow   int
	startCol   int

	renderer    *BrailleRenderer
}

func NewVideoPlayer(filename string, info *VideoInfo, termWidth, termHeight int) *VideoPlayer {
	charW := info.Width / 2
	charH := info.Height / 4

	startCol := (termWidth - charW) / 2
	startRow := (termHeight - charH) / 2
	if startCol < 0 {
		startCol = 0
	}
	if startRow < 0 {
		startRow = 0
	}

	fps := info.FPS
	if fps <= 0 {
		fps = 30
	}
	if fps > 60 {
		fps = 60
	}

	return &VideoPlayer{
		filename:    filename,
		videoInfo:   info,
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
	}
}

// launchDecoder 启动 ffmpeg 解码进程，返回 frame reader
func (vp *VideoPlayer) launchDecoder() io.ReadCloser {
	ffmpegPath := getFFmpegPath()
	pr, pw := io.Pipe()

	stream := ffmpeg.Input(vp.filename).
		Output("pipe:",
			ffmpeg.KwArgs{
				"format":   "rawvideo",
				"pix_fmt":  "rgba",
				"s":        fmt.Sprintf("%dx%d", vp.videoInfo.Width, vp.videoInfo.Height),
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

	return pr
}

// startLoop 循环播放，直到用户退出
func (vp *VideoPlayer) startLoop() {
	defer close(vp.done)

	// 信号监听（只处理 SIGWINCH，退出信号由按键处理）
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	// 键盘输入 goroutine
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
			// ESC 检测：区分 ESC 键和转义序列
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

	frameSize := vp.videoInfo.Width * vp.videoInfo.Height * 4

	// 主循环（循环播放）
	for {
		select {
		case <-vp.quit:
			return
		default:
		}

		// 启动解码器
		frameReader := vp.launchDecoder()
		fpsTicker := time.NewTicker(vp.frameTime)

		var outputBuf strings.Builder
		outputBuf.Grow(vp.charW*vp.charH*30 + vp.charH)

		// 每次循环开始时清屏，防止上一轮的残留
		fmt.Print(CLEAR_SCREEN + CURSOR_HOME)

		// 读取帧循环
	frameLoop:
		for {
			select {
			case <-vp.quit:
				fpsTicker.Stop()
				frameReader.Close()
				return
			case sig := <-sigCh:
				if sig == syscall.SIGWINCH {
					// 窗口变化：重新计算居中位置，清屏
					newSize, err := getTerminalSize()
					if err != nil {
						continue
					}
					newCharW := vp.videoInfo.Width / 2
					newCharH := vp.videoInfo.Height / 4
					newStartCol := (newSize.Width - newCharW) / 2
					newStartRow := (newSize.Height - newCharH) / 2
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

					// 清屏消除残留
					fmt.Print(CLEAR_SCREEN + CURSOR_HOME)
				}
			case key, ok := <-keyCh:
				if !ok {
					fpsTicker.Stop()
					frameReader.Close()
					return
				}
				if key == 'q' || key == 'Q' || key == 27 {
					fpsTicker.Stop()
					frameReader.Close()
					return
				}
			default:
			}

			// 读取一帧
			buf := make([]byte, frameSize)
			n, err := io.ReadFull(frameReader, buf)
			if err != nil || n != frameSize {
				// 视频结束 → 跳出帧循环，重新启动
				fpsTicker.Stop()
				frameReader.Close()
				break frameLoop
			}

			// 渲染
			imgData := rawRGBAToColorData(buf, vp.videoInfo.Width, vp.videoInfo.Height)
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

	// 计算输出尺寸
	outWidth, outHeight := calculateOutputSize(
		info.Width, info.Height,
		termSize.Width, termSize.Height,
	)
	info.Width = outWidth
	info.Height = outHeight

	// 创建播放器
	player := NewVideoPlayer(filename, info, termSize.Width, termSize.Height)

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
