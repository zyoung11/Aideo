package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	timage "Aideo/image"

	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/effects"
	"github.com/gopxl/beep/v2/speaker"
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

var cachedFFmpegPath string
var cachedHWAccel string
var hwAccelOnce sync.Once

func detectFFmpegAndHWAccel() (ffmpegPath string, hwAccel string) {
	hwAccelOnce.Do(func() {
		cachedFFmpegPath = getFFmpegPath()
	})
	return cachedFFmpegPath, ""
}

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
		return nil, fmt.Errorf("cannot probe video: %v", err)
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
	filename  string
	srcWidth  int
	srcHeight int
	outWidth  int
	outHeight int
	charW     int
	charH     int

	quit chan struct{}
	done chan struct{}

	fps         float64
	frameTime   time.Duration
	exposure    float64
	attenuation float64

	termWidth  int
	termHeight int
	startRow   int
	startCol   int

	proto       timage.Protocol
	cellW       int
	cellH       int

	sixelBuf   bytes.Buffer
	frameBuf   []byte
	sixelCache *timage.SixelFrameCache

	// 实时帧率统计
	fpsAccum           int64
	fpsAccumStart      time.Time
	displayFPS         float64

	// 自适应颜色
	colorCount int

	// 播放控制
	paused              bool
	totalFrames         int
	currentFrame        int64
	resumeSeekSecs      float64
	currentVolume       float64
	volumeDisplayTimer  int
	colorDisplayTimer   int
	seekSeconds         int
	lastFrame           []byte
	volumeStreamer      *effects.Volume
}

func NewVideoPlayer(filename string, srcWidth, srcHeight int, termWidth, termHeight int) *VideoPlayer {
	return &VideoPlayer{
		filename:      filename,
		srcWidth:      srcWidth,
		srcHeight:     srcHeight,
		termWidth:     termWidth,
		termHeight:    termHeight,
		quit:          make(chan struct{}),
		done:          make(chan struct{}),
		currentVolume: 1.0,
		seekSeconds:   DefaultSeekSeconds,
	}
}

// maxVideoDim 根据终端字符尺寸和单元格像素计算视频输出分辨率上限
// 目标：填满整个终端，不做分辨率压缩
func maxVideoDim(termChars, cellPx int) int {
	maxPx := (termChars - 2) * cellPx
	if maxPx < 320 {
		maxPx = 320
	}
	return maxPx
}

func (vp *VideoPlayer) initProto() {
	vp.cellW, vp.cellH = timage.CellPixels()
	if vp.cellW < 1 { vp.cellW = 8 }
	if vp.cellH < 1 { vp.cellH = 16 }
	vp.outWidth, vp.outHeight = calculateOutputSizeCells(vp.srcWidth, vp.srcHeight, vp.termWidth, vp.termHeight, vp.cellW, vp.cellH)
	maxPx := maxVideoDim(vp.termWidth, vp.cellW)
	vp.outWidth, vp.outHeight = capOutputSize(vp.outWidth, vp.outHeight, maxPx)
	vp.charW = (vp.outWidth + vp.cellW - 1) / vp.cellW
	vp.charH = (vp.outHeight + vp.cellH - 1) / vp.cellH
	vp.startCol = (vp.termWidth - vp.charW) / 2
	if vp.startCol < 0 { vp.startCol = 0 }
	vp.startRow = (vp.termHeight - vp.charH) / 2
	if vp.startRow < 0 { vp.startRow = 0 }
	totalStrips := (vp.outHeight + 5) / 6
	vp.sixelCache = timage.NewSixelFrameCache(totalStrips)
}

// updateTerminalSize 窗口变化时重新计算输出尺寸和居中位置
func (vp *VideoPlayer) updateTerminalSize(newWidth, newHeight int) {
	vp.termWidth = newWidth
	vp.termHeight = newHeight

	var newOutW, newOutH int
	newOutW, newOutH = calculateOutputSizeCells(vp.srcWidth, vp.srcHeight, newWidth, newHeight, vp.cellW, vp.cellH)
	maxPx := maxVideoDim(newWidth, vp.cellW)
	newOutW, newOutH = capOutputSize(newOutW, newOutH, maxPx)
	vp.charW = (newOutW + vp.cellW - 1) / vp.cellW
	vp.charH = (newOutH + vp.cellH - 1) / vp.cellH
	vp.outWidth = newOutW
	vp.outHeight = newOutH

	totalStrips := (vp.outHeight + 5) / 6
	vp.sixelCache = timage.NewSixelFrameCache(totalStrips)

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

// capOutputSize 限制输出分辨率最大边长，保持宽高比
func capOutputSize(w, h, maxPx int) (int, int) {
	if w <= maxPx && h <= maxPx {
		return w, h
	}
	if w > h {
		h = h * maxPx / w
		w = maxPx
	} else {
		w = w * maxPx / h
		h = maxPx
	}
	if w < 4 {
		w = 4
	}
	if h < 4 {
		h = 4
	}
	return w, h
}

// ==================== 视频解码器（双缓冲） ====================

type videoDecoder struct {
	reader    io.ReadCloser
	closeCh   chan struct{}
	doneCh    chan struct{}
	closeOnce sync.Once
}

// spawnVideoDecoder 启动一个 ffmpeg 视频解码进程
// seekSecs 为 0 表示从头开始，>0 表示从该秒附近开始解码
// 使用 output 端 -ss 实现精确 seek（fasteek），可确保输出的第一帧就是目标时间附近的帧
func (vp *VideoPlayer) spawnVideoDecoder(seekSecs float64) *videoDecoder {
	pr, pw := io.Pipe()

	outputArgs := ffmpeg.KwArgs{
		"format":  "rawvideo",
		"pix_fmt": "rgba",
		"s":       fmt.Sprintf("%dx%d", vp.outWidth, vp.outHeight),
		"an":      "",
		"sn":      "",
	}
	if seekSecs > 0 {
		outputArgs["ss"] = fmt.Sprintf("%.3f", seekSecs)
	}

	ffmpegPath, hwAccel := detectFFmpegAndHWAccel()

	var inputKwargs ffmpeg.KwArgs
	if hwAccel != "" {
		inputKwargs = ffmpeg.KwArgs{"hwaccel": hwAccel}
	}

	stream := ffmpeg.Input(vp.filename, inputKwargs).
		Output("pipe:", outputArgs).
		Silent(true).
		SetFfmpegPath(ffmpegPath).
		WithOutput(pw)

	dec := &videoDecoder{
		reader:  pr,
		closeCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	go func() {
		defer close(dec.doneCh)
		errCh := make(chan error, 1)
		go func() {
			errCh <- stream.Run()
		}()

		select {
		case <-dec.closeCh:
			pr.Close()
			<-errCh
		case err := <-errCh:
			if err != nil {
				pw.CloseWithError(fmt.Errorf("ffmpeg error: %v", err))
			} else {
				pw.Close()
			}
		}
	}()

	return dec
}

func (d *videoDecoder) close() {
	d.closeOnce.Do(func() {
		close(d.closeCh)
	})
	<-d.doneCh
}

// ==================== 音频解码器 ====================

const (
	audioSampleRate = 44100
	audioChannels   = 2
	audioBitDepth   = 2                             // s16le
	audioFrameSize  = audioChannels * audioBitDepth // 4 bytes per sample
)

// audioStreamer 实现 beep.Streamer，从 FFmpeg 的 raw PCM pipe 读取音频数据
// 支持循环播放（双缓冲），也支持静音降噪
type audioStreamer struct {
	mu         sync.Mutex
	filename   string
	ffmpegPath string
	outWidth   int
	outHeight  int

	currentReader io.ReadCloser
	nextReader    io.ReadCloser

	closeCh chan struct{}
	closed  bool

	err error
}

func newAudioStreamer(filename string) *audioStreamer {
	ffmpegPath, _ := detectFFmpegAndHWAccel()
	return &audioStreamer{
		filename:   filename,
		ffmpegPath: ffmpegPath,
		closeCh:    make(chan struct{}),
	}
}

func (as *audioStreamer) spawnAudioDecoder() io.ReadCloser {
	pr, pw := io.Pipe()

	stream := ffmpeg.Input(as.filename).
		Output("pipe:",
			ffmpeg.KwArgs{
				"format": "s16le",
				"acodec": "pcm_s16le",
				"ac":     audioChannels,
				"ar":     audioSampleRate,
				"vn":     "",
				"sn":     "",
			}).
		Silent(true).
		SetFfmpegPath(as.ffmpegPath).
		WithOutput(pw)

	go func() {
		err := stream.Run()
		if err != nil {
			pw.CloseWithError(fmt.Errorf("audio decode error: %v", err))
		} else {
			pw.Close()
		}
	}()

	return pr
}

// ensureNext 确保 have a next reader ready
func (as *audioStreamer) ensureNext() {
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.nextReader == nil && !as.closed {
		as.nextReader = as.spawnAudioDecoder()
	}
}

// Stream 实现 beep.Streamer.Stream
func (as *audioStreamer) Stream(samples [][2]float64) (n int, ok bool) {
	as.mu.Lock()
	defer as.mu.Unlock()

	if as.closed {
		return 0, false
	}

	// 首次调用时启动第一个解码器
	if as.currentReader == nil {
		as.currentReader = as.spawnAudioDecoder()
		as.nextReader = as.spawnAudioDecoder()
	}

	buf := make([]byte, len(samples)*audioFrameSize)

	offset := 0
	for offset < len(buf) {
		nRead, err := as.currentReader.Read(buf[offset:])
		offset += nRead
		if err != nil {
			// 当前解码器结束，切换到下一个
			if as.currentReader != nil {
				as.currentReader.Close()
			}
			as.currentReader = as.nextReader
			as.nextReader = as.spawnAudioDecoder()

			if as.currentReader == nil {
				as.err = fmt.Errorf("音频解码器不可用")
				return 0, false
			}

			// 继续读新解码器的数据
			offset = 0 // 抛弃之前读的部分，保持同步
			continue
		}
	}

	// 将 s16le bytes 转换为 float64 samples
	for i := range samples {
		if i*audioFrameSize+audioFrameSize > len(buf) {
			break
		}
		left := int16(binary.LittleEndian.Uint16(buf[i*audioFrameSize:]))
		right := int16(binary.LittleEndian.Uint16(buf[i*audioFrameSize+2:]))
		samples[i][0] = float64(left) / math.MaxInt16
		samples[i][1] = float64(right) / math.MaxInt16
	}

	return len(samples), true
}

func (as *audioStreamer) Err() error {
	as.mu.Lock()
	defer as.mu.Unlock()
	return as.err
}

func (as *audioStreamer) Close() {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.closed = true
	if as.currentReader != nil {
		as.currentReader.Close()
	}
	if as.nextReader != nil {
		as.nextReader.Close()
	}
}

// noopAudioStreamer 是一个静默的音频流，用于无音轨的视频，避免 speaker 相关死锁
type noopAudioStreamer struct{}

func (n *noopAudioStreamer) Stream(samples [][2]float64) (int, bool) {
	for i := range samples {
		samples[i][0] = 0
		samples[i][1] = 0
	}
	return len(samples), true
}

func (n *noopAudioStreamer) Err() error { return nil }
func (n *noopAudioStreamer) Close()     {}

// ==================== 播放循环 ====================



func (vp *VideoPlayer) renderSixelFrame(raw []byte) {
	vp.sixelBuf.Reset()
	fmt.Fprintf(&vp.sixelBuf, "\033[%d;%dH", vp.startRow+1, vp.startCol+1)
	encStart := time.Now()
	timage.EncodeSixelFrameRaw(&vp.sixelBuf, raw, vp.outWidth, vp.outHeight, vp.colorCount, false, vp.sixelCache)
	encDur := time.Since(encStart)

	// 清除图像右侧的残留区域
	if vp.charW > 0 && vp.startCol+vp.charW <= vp.termWidth {
		clearStartCol := vp.startCol + vp.charW + 1
		for row := vp.startRow + 1; row <= vp.startRow+vp.charH; row++ {
			fmt.Fprintf(&vp.sixelBuf, "\033[%d;%dH\033[K", row, clearStartCol)
		}
	}

	// 清除图像下方的残留区域
	if vp.startRow+vp.charH < vp.termHeight {
		fmt.Fprintf(&vp.sixelBuf, "\033[%d;1H\033[J", vp.startRow+vp.charH+1)
	}

	vp.renderControlBar(&vp.sixelBuf, encDur)
	os.Stdout.Write(vp.sixelBuf.Bytes())
}



func (vp *VideoPlayer) cleanupFrame() {
	fmt.Print("\033[2J\033[3J\033[H")
}

// updateFPS 每帧调用，累计帧数并更新实时帧率（每 500ms 刷新一次）
func (vp *VideoPlayer) updateFPS() {
	vp.fpsAccum++
	now := time.Now()
	if vp.fpsAccumStart.IsZero() {
		vp.fpsAccumStart = now
		return
	}
	elapsed := now.Sub(vp.fpsAccumStart)
	if elapsed >= 500*time.Millisecond {
		vp.displayFPS = float64(vp.fpsAccum) / elapsed.Seconds()
		vp.fpsAccum = 0
		vp.fpsAccumStart = now
	}
}

func (vp *VideoPlayer) renderControlBar(buf *bytes.Buffer, encDur time.Duration) {
	w := vp.termWidth
	h := vp.termHeight

	infoRow := h - 2
	barRow := h - 1

	buf.WriteString(fmt.Sprintf("\033[%d;1H\033[K", infoRow))

	if vp.volumeDisplayTimer > 0 {
		volPct := int(vp.currentVolume * 100)
		volStr := fmt.Sprintf("%d%%", volPct)
		buf.WriteString(fmt.Sprintf("\033[%d;%dH\033[0m%s", infoRow, 1, volStr))
	}

	if vp.colorDisplayTimer > 0 {
		colorStr := fmt.Sprintf("%dc", vp.colorCount)
		colorW := displayWidth(colorStr)
		buf.WriteString(fmt.Sprintf("\033[%d;%dH\033[0m%s", infoRow, w-colorW, colorStr))
	}

	infoCenter := fmt.Sprintf("[ %dx%d | %.1ffps | %dc ]", vp.outWidth, vp.outHeight, vp.displayFPS, vp.colorCount)
	infoW := displayWidth(infoCenter)
	infoX := (w - infoW) / 2
	if infoX < 1 {
		infoX = 1
	}
	buf.WriteString(fmt.Sprintf("\033[%d;%dH\033[90m%s\033[0m", infoRow, infoX, infoCenter))

	buf.WriteString(fmt.Sprintf("\033[%d;1H\033[K", barRow))

	icon := "⏸"
	if vp.paused {
		icon = "▶"
	}
	buf.WriteString(fmt.Sprintf("\033[0m%s ", icon))

	modeIcon := "⟳"
	modeW := displayWidth(modeIcon)
	barWidth := w - 5 - modeW
	if barWidth < 4 {
		barWidth = 4
	}

	progress := 0.0
	if vp.totalFrames > 0 {
		progress = float64(vp.currentFrame) / float64(vp.totalFrames)
	}
	if progress > 1.0 {
		progress = 1.0
	}
	playedChars := int(float64(barWidth) * progress)

	buf.WriteString("\033[2m")
	for i := 0; i < playedChars; i++ {
		buf.WriteString("━")
	}
	buf.WriteString("\033[0m\033[0m")
	for i := playedChars; i < barWidth; i++ {
		buf.WriteString("━")
	}

	buf.WriteString(fmt.Sprintf("\033[0m \033[90m%s\033[0m", modeIcon))

	_ = encDur
}

func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if r >= 0x2E80 && r <= 0x9FFF || r >= 0xF900 && r <= 0xFAFF || r >= 0xFF01 && r <= 0xFF60 {
			w += 2
		} else {
			w += 1
		}
	}
	return w
}

// startLoop 循环播放，使用双缓冲 decoder 实现无缝循环
func (vp *VideoPlayer) startLoop() {
	defer close(vp.done)

	// 信号监听
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	// 键盘输入 goroutine — 使用 os.Stdin.Read（非缓冲），参考 main.go 中图片模式的实现
	keyCh := make(chan byte, 10)
	go func() {
		defer func() { recover() }()

		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				close(keyCh)
				return
			}

			for i := 0; i < n; i++ {
				b := buf[i]

				if b == 27 && i+2 < n && buf[i+1] == '[' {
					term := buf[i+2]
					switch term {
					case 'A':
						select { case keyCh <- 0x80: default: }
					case 'B':
						select { case keyCh <- 0x81: default: }
					case 'C':
						select { case keyCh <- 0x82: default: }
					case 'D':
						select { case keyCh <- 0x83: default: }
					}
					i += 2
					continue
				}

				if b == 27 && i+1 < n {
					next := buf[i+1]
					if next == '[' || next == 'O' || next == 'M' {
						i += 2
						for i < n {
							ch := buf[i]
							i++
							if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '~' {
								break
							}
						}
						i--
						continue
					}
				}

				select {
				case keyCh <- b:
				default:
				}
			}
		}
	}()

	hasAudio := hasAudioStream(vp.filename)

	var rawStream beep.Streamer
	var closer func()
	if hasAudio {
		s := newAudioStreamer(vp.filename)
		rawStream = s
		closer = func() { s.Close() }
	} else {
		s := &noopAudioStreamer{}
		rawStream = s
		closer = func() { s.Close() }
	}

	vp.volumeStreamer = &effects.Volume{Streamer: rawStream, Base: 2, Volume: 0}

	speakerInitialized := false
	err := speaker.Init(beep.SampleRate(audioSampleRate), audioSampleRate/10)
	if err == nil {
		speakerInitialized = true
		speaker.Play(beep.Seq(vp.volumeStreamer, beep.Callback(func() {})))
	}

	// 帧计数和当前播放时间（用于 resize seek）
	var frameCount int64
	var renderFrame []byte

	// 启动第一个视频解码器
	currentDec := vp.spawnVideoDecoder(0)
	// 预启动第二个视频解码器（双缓冲）
	nextDec := vp.spawnVideoDecoder(0)

	// 帧读取 goroutine
	frameCh := make(chan []byte, 2)
	stopCh := make(chan struct{})
	decSwitchCh := make(chan struct {
		current *videoDecoder
		next    *videoDecoder
	}, 1)
	go func() {
		cd := currentDec
		nd := nextDec
		for {
			select {
			case sw := <-decSwitchCh:
				cd.close()
				if nd != sw.current && nd != sw.next {
					nd.close()
				}
				cd = sw.current
				nd = sw.next
				continue
			default:
			}
			fs := vp.outWidth * vp.outHeight * 4
			if cap(vp.frameBuf) < fs {
				vp.frameBuf = make([]byte, fs)
			}
			n, err := io.ReadFull(cd.reader, vp.frameBuf[:fs])
			if err != nil || n != fs {
				cd.close()
				cd = nd
				nd = vp.spawnVideoDecoder(vp.resumeSeekSecs)
				continue
			}
			fc := make([]byte, fs)
			copy(fc, vp.frameBuf[:fs])
			select {
			case frameCh <- fc:
			case <-stopCh:
				return
			}
		}
	}()

	// 清屏一次（只在最开始时）
	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)

	playbackStart := time.Now()

	// 窗口 resize 防抖
	var resizePending bool
	resizeTimer := time.NewTimer(0)
	if !resizeTimer.Stop() {
		<-resizeTimer.C
	}
	defer resizeTimer.Stop()

	// 主帧循环
	for {
		select {
		case <-vp.quit:
			closer()
			if speakerInitialized {
				speaker.Close()
			}
			close(stopCh)
			currentDec.close()
			nextDec.close()
			vp.cleanupFrame()
			return
		case <-sigCh:
			// 标记 resize 待处理，通过防抖避免频繁重建
			resizePending = true
			resizeTimer.Reset(300 * time.Millisecond)
			continue
		case <-resizeTimer.C:
			if !resizePending {
				continue
			}
			resizePending = false

			currentTime := float64(frameCount) / vp.fps

			// 精确 seek 到当前时间往前 0.15 秒（output 端 -ss 会解码到目标位置输出）
			seekTime := currentTime - 0.05
			if seekTime < 0 {
				seekTime = 0
			}

			// 关闭旧的视频解码器
			currentDec.close()
			nextDec.close()

			// 更新终端尺寸
			vp.updateTerminalSizeFromSigwinch()
			// 重新计算 frameSize

			// 从 seekTime 处启动新的解码器
			newCurrent := vp.spawnVideoDecoder(seekTime)
			newNext := vp.spawnVideoDecoder(seekTime)
			decSwitchCh <- struct {
				current *videoDecoder
				next    *videoDecoder
			}{newCurrent, newNext}

			seekFrame := int64(seekTime * vp.fps)
			if seekFrame < 0 {
				seekFrame = 0
			}
			frameCount = int64(seekTime * vp.fps)
			if frameCount < 0 {
				frameCount = 0
			}
			renderFrame = nil

			drainLoop:
			for {
				select {
				case <-frameCh:
				default:
					break drainLoop
				}
			}

			// 清屏 + 定位光标到左上角 (1,1)
			fmt.Print(CLEAR_SCREEN + "\x1b[1;1H")
			continue
		case key, ok := <-keyCh:
			if !ok {
				closer()
				if speakerInitialized {
					speaker.Close()
				}
				currentDec.close()
				nextDec.close()
				vp.cleanupFrame()
				return
			}
			if key == 'q' || key == 'Q' || key == 27 {
				closer()
				if speakerInitialized {
					speaker.Close()
				}
				currentDec.close()
				nextDec.close()
				vp.cleanupFrame()
				return
			}
			if key == ' ' {
				vp.paused = !vp.paused
				continue
			}
			if key == 'w' || key == 'k' {
				vp.colorDisplayTimer = 10
				levels := []int{2, 8, 16, 32, 64, 128, 256}
				for _, lvl := range levels {
					if lvl > vp.colorCount {
						vp.colorCount = lvl
						vp.sixelCache = nil
						break
					}
				}
				continue
			}
			if key == 's' || key == 'j' {
				vp.colorDisplayTimer = 10
				levels := []int{2, 8, 16, 32, 64, 128, 256}
				for i := len(levels) - 1; i >= 0; i-- {
					if levels[i] < vp.colorCount {
						vp.colorCount = levels[i]
						vp.sixelCache = nil
						break
					}
				}
				continue
			}
			if key == 0x80 {
				vp.volumeDisplayTimer = 10
				vp.currentVolume = min(vp.currentVolume+0.05, 1.0)
				if vp.volumeStreamer != nil {
					speaker.Lock()
					vp.volumeStreamer.Volume = math.Log2(vp.currentVolume)
					if vp.currentVolume == 0 {
						vp.volumeStreamer.Volume = -10
					}
					speaker.Unlock()
				}
				continue
			}
			if key == 0x81 {
				vp.volumeDisplayTimer = 10
				vp.currentVolume = max(vp.currentVolume-0.05, 0.0)
				if vp.volumeStreamer != nil {
					speaker.Lock()
					vp.volumeStreamer.Volume = math.Log2(vp.currentVolume)
					if vp.currentVolume == 0 {
						vp.volumeStreamer.Volume = -10
					}
					speaker.Unlock()
				}
				continue
			}
		case renderFrame = <-frameCh:
			if renderFrame == nil {
				closer()
				if speakerInitialized {
					speaker.Close()
				}
				vp.cleanupFrame()
				return
			}
			expectedSize := vp.outWidth * vp.outHeight * 4
			if len(renderFrame) != expectedSize {
				continue
			}
		}

		if vp.paused {
			vp.lastFrame = renderFrame
			pauseStart := time.Now()
			if speakerInitialized {
				speaker.Lock()
			}
			vp.pausedLoop(frameCh, keyCh, sigCh, resizeTimer, closer, speakerInitialized, currentDec, nextDec, decSwitchCh)
			if speakerInitialized {
				speaker.Unlock()
			}
			playbackStart = playbackStart.Add(time.Since(pauseStart))
			continue
		}

		vp.lastFrame = renderFrame
		frameCount++

		if vp.volumeDisplayTimer > 0 {
			vp.volumeDisplayTimer--
		}
		if vp.colorDisplayTimer > 0 {
			vp.colorDisplayTimer--
		}

		vp.updateFPS()

		vp.currentFrame = frameCount
		vp.resumeSeekSecs = float64(frameCount) / vp.fps

		// A/V sync: drop frames if behind audio
		syncLoop:
		for {
			expectedTime := playbackStart.Add(time.Duration(float64(frameCount)) * vp.frameTime)
			if time.Since(expectedTime) <= vp.frameTime/2 {
				break
			}
			select {
			case renderFrame = <-frameCh:
				if renderFrame == nil {
					closer()
					if speakerInitialized {
						speaker.Close()
					}
					vp.cleanupFrame()
					return
				}
				if len(renderFrame) != vp.outWidth*vp.outHeight*4 {
					continue
				}
			default:
				break syncLoop
			}
			frameCount++
		}

		vp.renderSixelFrame(renderFrame)

		expectedTime := playbackStart.Add(time.Duration(float64(frameCount)) * vp.frameTime)
		if remaining := time.Until(expectedTime); remaining > 0 {
			time.Sleep(remaining)
		}
	}
}

// updateTerminalSizeFromSigwinch 从系统获取当前终端尺寸并更新
func (vp *VideoPlayer) pausedLoop(frameCh chan []byte, keyCh chan byte, sigCh chan os.Signal, resizeTimer *time.Timer, closer func(), speakerInitialized bool, currentDec, nextDec *videoDecoder, decSwitchCh chan struct {
		current *videoDecoder
		next    *videoDecoder
	}) {

drainLoop:
	for {
		select {
		case f := <-frameCh:
			vp.lastFrame = f
		default:
			break drainLoop
		}
	}

	for {
		vp.renderSixelFrame(vp.lastFrame)

		select {
		case <-vp.quit:
			return
		case sig := <-sigCh:
			if sig == syscall.SIGWINCH {
				vp.pausedResize(frameCh, currentDec, nextDec, decSwitchCh)
			}
			continue
		case <-resizeTimer.C:
			vp.pausedResize(frameCh, currentDec, nextDec, decSwitchCh)
			continue
		case key, ok := <-keyCh:
			if !ok {
				close(vp.quit)
				return
			}
			if key == 'q' || key == 'Q' || key == 27 {
				vp.paused = false
				close(vp.quit)
				return
			}
			if key == ' ' {
				vp.paused = false
				return
			}
			if key == 'w' || key == 'k' {
				vp.colorDisplayTimer = 10
				levels := []int{2, 8, 16, 32, 64, 128, 256}
				for _, lvl := range levels {
					if lvl > vp.colorCount {
						vp.colorCount = lvl
						vp.sixelCache = nil
						break
					}
				}
				continue
			}
			if key == 's' || key == 'j' {
				vp.colorDisplayTimer = 10
				levels := []int{2, 8, 16, 32, 64, 128, 256}
				for i := len(levels) - 1; i >= 0; i-- {
					if levels[i] < vp.colorCount {
						vp.colorCount = levels[i]
						vp.sixelCache = nil
						break
					}
				}
				continue
			}
			if key == 0x80 {
				vp.volumeDisplayTimer = 10
				vp.currentVolume = min(vp.currentVolume+0.05, 1.0)
				if vp.volumeStreamer != nil {
					speaker.Lock()
					vp.volumeStreamer.Volume = math.Log2(vp.currentVolume)
					if vp.currentVolume == 0 {
						vp.volumeStreamer.Volume = -10
					}
					speaker.Unlock()
				}
				continue
			}
			if key == 0x81 {
				vp.volumeDisplayTimer = 10
				vp.currentVolume = max(vp.currentVolume-0.05, 0.0)
				if vp.volumeStreamer != nil {
					speaker.Lock()
					vp.volumeStreamer.Volume = math.Log2(vp.currentVolume)
					if vp.currentVolume == 0 {
						vp.volumeStreamer.Volume = -10
					}
					speaker.Unlock()
				}
				continue
			}
		case <-time.After(33 * time.Millisecond):
		}
	}
}

func (vp *VideoPlayer) pausedResize(frameCh chan []byte, currentDec, nextDec *videoDecoder, decSwitchCh chan struct {
		current *videoDecoder
		next    *videoDecoder
	}) {
	oldW, oldH := vp.outWidth, vp.outHeight
	vp.updateTerminalSizeFromSigwinch()

	vp.lastFrame = rescaleRGBA(vp.lastFrame, oldW, oldH, vp.outWidth, vp.outHeight)
	totalStrips := (vp.outHeight + 5) / 6
	vp.sixelCache = timage.NewSixelFrameCache(totalStrips)

	newDec := vp.spawnVideoDecoder(vp.resumeSeekSecs)
	decSwitchCh <- struct {
		current *videoDecoder
		next    *videoDecoder
	}{newDec, vp.spawnVideoDecoder(vp.resumeSeekSecs)}

	time.Sleep(50 * time.Millisecond)

	for {
		select {
		case <-frameCh:
		default:
			goto drainDone
		}
	}
drainDone:

	fmt.Print(CLEAR_SCREEN + "\x1b[1;1H")
}

func (vp *VideoPlayer) updateTerminalSizeFromSigwinch() {
	newSize, err := getTerminalSize()
	if err != nil {
		return
	}
	vp.updateTerminalSize(newSize.Width, newSize.Height)
}

func (vp *VideoPlayer) updateTerminalSizeLight() {
	newSize, err := getTerminalSize()
	if err != nil {
		return
	}
	vp.termWidth = newSize.Width
	vp.termHeight = newSize.Height
	vp.startCol = (vp.termWidth - vp.charW) / 2
	if vp.startCol < 0 {
		vp.startCol = 0
	}
	vp.startRow = (vp.termHeight - vp.charH) / 2
	if vp.startRow < 0 {
		vp.startRow = 0
	}
}

func rescaleRGBA(data []byte, oldW, oldH, newW, newH int) []byte {
	if oldW == newW && oldH == newH {
		return data
	}
	out := make([]byte, newW*newH*4)
	for y := 0; y < newH; y++ {
		srcY := y * oldH / newH
		for x := 0; x < newW; x++ {
			srcX := x * oldW / newW
			srcOff := (srcY*oldW + srcX) * 4
			dstOff := (y*newW + x) * 4
			copy(out[dstOff:dstOff+4], data[srcOff:srcOff+4])
		}
	}
	return out
}

// ==================== 音频流检测 ====================

func hasAudioStream(filename string) bool {
	probeData, err := ffmpeg.Probe(filename)
	if err != nil {
		// cannot probe, assume has audio (safe default)
		return true
	}

	type ProbeStream struct {
		CodecType string `json:"codec_type"`
	}
	type ProbeResult struct {
		Streams []ProbeStream `json:"streams"`
	}

	var probe ProbeResult
	if err := json.Unmarshal([]byte(probeData), &probe); err != nil {
		return true
	}

	for _, s := range probe.Streams {
		if s.CodecType == "audio" {
			return true
		}
	}
	return false
}

// ==================== 播放入口 ====================

func playVideo(filename string, proto timage.Protocol) error {
	termSize, err := getTerminalSize()
	if err != nil {
		return fmt.Errorf("获取终端尺寸失败: %v", err)
	}

	useProto := proto != timage.ProtocolAuto
	if !useProto {
		fmt.Print(ENTER_ALTERNATE)
	}
	fmt.Print(HIDE_CURSOR)
	fmt.Print(DISABLE_MOUSE)
	defer func() {
		fmt.Print(ENABLE_MOUSE)
		fmt.Print(SHOW_CURSOR)
		if !useProto {
			fmt.Print(LEAVE_ALTERNATE)
		}
	}()

	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)
	fmt.Printf("\033[%d;%dH\033[90m正在探测视频...%s",
		termSize.Height/2, termSize.Width/2-10, RESET_COLORS)

	info, err := probeVideo(filename)
	if err != nil {
		return fmt.Errorf("视频探测失败: %v", err)
	}

	player := NewVideoPlayer(filename, info.Width, info.Height, termSize.Width, termSize.Height)
	player.fps = info.FPS
	player.frameTime = time.Duration(float64(time.Second) / info.FPS)
	player.totalFrames = info.TotalFrames
	player.proto = proto
	player.colorCount = 64
	player.initProto()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("cannot set raw mode: %v", err)
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

func initVideoPlayback(filename string, proto timage.Protocol) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Print(ENABLE_MOUSE)
			fmt.Print(SHOW_CURSOR)
			fmt.Print(LEAVE_ALTERNATE)
			fmt.Fprintf(os.Stderr, "视频播放发生 panic: %v\n", r)
			os.Exit(1)
		}
	}()

	err := playVideo(filename, proto)
	if err != nil {
		fmt.Print(ENABLE_MOUSE)
		fmt.Print(SHOW_CURSOR)
		fmt.Print(LEAVE_ALTERNATE)
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
