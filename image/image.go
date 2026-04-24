package image

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"math"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nfnt/resize"
	"github.com/soniakeys/quant/median"
	"golang.org/x/term"
)

type Protocol int

const (
	ProtocolAuto Protocol = iota
	ProtocolSixel
	ProtocolKitty
)

type Placeholder struct {
	Col    int
	Row    int
	Cols   int
	Rows   int
	PixelW int
	PixelH int
}

type ColorRGB struct {
	R int
	G int
	B int
}

type Placement struct {
	// 图像左上角 X 位置（占终端宽度的比例，0.0 = 左边缘，1.0 = 右边缘）
	Left float64
	// 图像左上角 Y 位置（占终端高度的比例，0.0 = 上边缘，1.0 = 下边缘）
	Top float64
	// 图像宽度（占终端宽度的比例，0.0 ~ 1.0）
	Width float64
	// 图像高度（占终端高度的比例，0.0 ~ 1.0）
	Height float64
}

type DisplayConfig struct {
	// 图像数据（RGBA格式）
	Img *image.RGBA

	// 图像在屏幕中的位置和尺寸（基于终端比例，0.0~1.0）
	// 如果为 nil，默认居中占满整个屏幕
	Placement *Placement

	// 保持图像原始纵横比（默认 true）
	// 为 false 时图像会被拉伸到 Placement 指定的区域
	KeepAspectRatio bool

	// 距左右边缘的最小留白（字符数）
	PadCol int
	// 距上下边缘的最小留白（字符数）
	PadRow int

	// Sixel 颜色数（默认 255）
	SixelColors int
	// Sixel 是否启用抖动
	SixelDither bool

	// 强制协议（0=自动检测）
	ForceProtocol Protocol

	// 是否分析封面主色调（默认 true）
	DisableColorAnalysis bool

	// 终端字符像素尺寸（0=自动检测）
	CellW int
	CellH int
}

type DisplayResult struct {
	Placeholder Placeholder
	Dominant    ColorRGB
}

var kittyImageID uint32 = uint32(os.Getpid()<<16) + uint32(time.Now().UnixMicro()&0xFFFF)

// kittyZlibPool 复用 zlib.Writer，减少每帧的分配开销
var kittyZlibPool = sync.Pool{
	New: func() interface{} {
		w, _ := zlib.NewWriterLevel(nil, zlib.BestSpeed)
		return w
	},
}

// kittyCompressPool 复用压缩用的 bytes.Buffer
var kittyCompressPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// kittyBase64Pool 复用 base64 编码缓冲区，避免每帧分配大 []byte
var kittyBase64Pool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 256*1024) // 256KB 初始容量
		return buf
	},
}

var termGetSize = func(fd int) (int, int, error) {
	return term.GetSize(fd)
}

func DefaultConfig() DisplayConfig {
	return DisplayConfig{
		KeepAspectRatio: true,
		SixelColors:     255,
		SixelDither:     false,
		PadCol:          1,
		PadRow:          1,
	}
}

func ShowImage(cfg DisplayConfig) (*DisplayResult, error) {
	if cfg.Img == nil {
		return nil, fmt.Errorf("image is nil")
	}

	bounds := cfg.Img.Bounds()
	origW, origH := bounds.Dx(), bounds.Dy()
	if origW == 0 || origH == 0 {
		return nil, fmt.Errorf("image has zero dimensions")
	}

	cellW, cellH := cfg.CellW, cfg.CellH
	if cellW <= 0 || cellH <= 0 {
		cellW, cellH = queryCellPixels()
		if cellW <= 0 {
			cellW = 8
		}
		if cellH <= 0 {
			cellH = 16
		}
	}

	p := cfg.Placement
	if p == nil {
		p = &Placement{Left: 0.5, Top: 0.5, Width: 1.0, Height: 1.0}
	}

	protocol := cfg.ForceProtocol
	if protocol == ProtocolAuto {
		protocol = detectProtocol()
	}

	// 设 raw 模式以读取键盘
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("make raw: %v", err)
	}
	defer term.Restore(fd, oldState)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	buf := make([]byte, 1)
	keyCh := make(chan byte, 1)
	go func() {
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			keyCh <- buf[0]
		}
	}()

	var lastResult *DisplayResult
	lastW, lastH := 0, 0
	done := false

	for !done {
		select {
		case <-winch:
		case key := <-keyCh:
			if key == 'q' || key == 'Q' || key == 0x1b || key == 0x03 {
				done = true
				continue
			}
			continue
		default:
		}

		termW, termH, err := termGetSize(int(os.Stdout.Fd()))
		if err != nil {
			return nil, fmt.Errorf("get terminal size: %v", err)
		}
		if lastResult != nil && termW == lastW && termH == lastH {
			continue
		}
		lastW, lastH = termW, termH

		termPixelW := termW * cellW
		termPixelH := termH * cellH

		areaPixelW := int(float64(termPixelW) * p.Width)
		areaPixelH := int(float64(termPixelH) * p.Height)
		if areaPixelW < 10 {
			areaPixelW = 10
		}
		if areaPixelH < 10 {
			areaPixelH = 10
		}

		// 先确定目标字符数
		targetCols := areaPixelW / cellW
		targetRows := areaPixelH / cellH
		if targetCols > termW {
			targetCols = termW
		}
		if targetRows > termH {
			targetRows = termH
		}
		if targetCols < 1 {
			targetCols = 1
		}
		if targetRows < 1 {
			targetRows = 1
		}

		// 缩放目标像素对齐到 cell 整数倍
		targetPixelW := targetCols * cellW
		targetPixelH := targetRows * cellH

		var scaled image.Image
		if cfg.KeepAspectRatio {
			scaled = resize.Thumbnail(uint(targetPixelW), uint(targetPixelH), cfg.Img, resize.Lanczos3)
		} else {
			scaled = resize.Resize(uint(targetPixelW), uint(targetPixelH), cfg.Img, resize.Lanczos3)
		}

		finalW, finalH := scaled.Bounds().Dx(), scaled.Bounds().Dy()

		// 像素 → 字符（向上取整）
		imageCols := (finalW + cellW - 1) / cellW
		imageRows := (finalH + cellH - 1) / cellH
		if imageCols > targetCols {
			imageCols = targetCols
		}
		if imageRows > targetRows {
			imageRows = targetRows
		}
		if imageCols < 1 {
			imageCols = 1
		}
		if imageRows < 1 {
			imageRows = 1
		}

		startCol := int(float64(termW)*p.Left+0.5) - imageCols/2
		startRow := int(float64(termH)*p.Top+0.5) - imageRows/2
		if startCol+imageCols > termW {
			startCol = termW - imageCols
		}
		if startRow+imageRows > termH {
			startRow = termH - imageRows
		}
		if startCol < 0 {
			startCol = 0
		}
		if startRow < 0 {
			startRow = 0
		}
		startCol++
		startRow++

		if lastResult == nil {
			fmt.Print("\x1b[?25l")
		} else {
			if protocol == ProtocolKitty {
				clearKittyAll()
			}
		}
		fmt.Print("\x1b[2J\x1b[3J\x1b[H")

		fmt.Printf("\x1b[%d;%dH", startRow, startCol)

		switch protocol {
		case ProtocolKitty:
			err = renderKitty(scaled, imageCols, imageRows)
		default:
			err = renderSixel(scaled, cfg.SixelColors, cfg.SixelDither)
		}
		if err != nil {
			return nil, fmt.Errorf("render: %v", err)
		}

		fillCol := startCol + imageCols
		if fillCol <= termW {
			for row := startRow; row < startRow+imageRows; row++ {
				fmt.Printf("\x1b[%d;%dH\x1b[K", row, fillCol)
			}
		}
		// 清除图像下方区域，防止 Sixel 残余像素
		if startRow+imageRows < termH {
			fmt.Printf("\x1b[%d;%dH\x1b[J", startRow+imageRows, startCol)
		}

		lastResult = &DisplayResult{
			Placeholder: Placeholder{
				Col:    startCol,
				Row:    startRow,
				Cols:   imageCols,
				Rows:   imageRows,
				PixelW: finalW,
				PixelH: finalH,
			},
		}

		if !cfg.DisableColorAnalysis {
			r, g, b := analyzeDominant(scaled)
			lastResult.Dominant = ColorRGB{R: r, G: g, B: b}
		}
	}

	fmt.Print("\x1b[?25h")
	return lastResult, nil
}

func clearKittyAll() {
	fmt.Print("\x1b_Ga=d\x1b\\")
}

func ClearImage(protocol Protocol) error {
	fmt.Print("\x1b[2J\x1b[3J\x1b[H")
	if protocol == ProtocolKitty {
		fmt.Print("\x1b_Ga=d\x1b\\")
	}
	return nil
}

func ColorCode(c ColorRGB, bold bool) string {
	if bold {
		return fmt.Sprintf("\x1b[1;38;2;%d;%d;%dm", c.R, c.G, c.B)
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.R, c.G, c.B)
}

func Temperature(c ColorRGB) string {
	brightness := 0.2126*float64(c.R) + 0.7152*float64(c.G) + 0.0722*float64(c.B)
	if brightness > 130 {
		return "light"
	}
	return "dark"
}

func detectProtocol() Protocol {
	termProgram := os.Getenv("TERM_PROGRAM")
	termName := strings.ToLower(os.Getenv("TERM"))

	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return ProtocolKitty
	}
	if termProgram == "WezTerm" || termProgram == "ghostty" || termProgram == "rio" {
		return ProtocolKitty
	}
	if os.Getenv("WEZTERM_EXECUTABLE") != "" {
		return ProtocolKitty
	}
	if strings.Contains(termName, "kitty") {
		return ProtocolKitty
	}
	return ProtocolSixel
}

func queryCellPixels() (int, int) {
	termW, termH, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 8, 16
	}
	if termW <= 0 || termH <= 0 {
		return 8, 16
	}
	// 先查询终端总像素尺寸，然后除以字符数得到单元格像素
	pixelW, pixelH := queryTerminalPixels()
	if pixelW <= 0 || pixelH <= 0 || termW <= 0 {
		return 8, 16
	}
	cw := pixelW / termW
	ch := pixelH / termH
	if cw < 4 {
		cw = 8
	}
	if ch < 4 {
		ch = 16
	}
	return cw, ch
}

func CellPixels() (int, int) {
	return queryCellPixels()
}

func queryTerminalPixels() (int, int) {
	makeRaw := func() func() {
		fd := int(os.Stdin.Fd())
		old, err := term.MakeRaw(fd)
		if err != nil {
			return func() {}
		}
		return func() { term.Restore(fd, old) }
	}

	restore := makeRaw()
	defer restore()

	fmt.Print("\x1b[14t")

	var buf []byte
	var b [1]byte
	timeout := time.After(100 * time.Millisecond)
	done := make(chan struct{}, 1)

	go func() {
		for {
			n, err := os.Stdin.Read(b[:])
			if err != nil || n == 0 {
				return
			}
			buf = append(buf, b[0])
			if b[0] == 't' {
				done <- struct{}{}
				return
			}
		}
	}()

	select {
	case <-done:
	case <-timeout:
		return 0, 0
	}

	if len(buf) < 4 || buf[0] != '\x1b' || buf[1] != '[' || buf[len(buf)-1] != 't' {
		return 0, 0
	}
	parts := bytes.Split(buf[2:len(buf)-1], []byte(";"))
	if len(parts) < 3 {
		return 0, 0
	}
	h := 0
	w := 0
	fmt.Sscanf(string(parts[1]), "%d", &h)
	fmt.Sscanf(string(parts[2]), "%d", &w)
	return w, h
}

func analyzeDominant(img image.Image) (int, int, int) {
	bounds := img.Bounds()
	colorCount := make(map[[3]int]int)

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pr, pg, pb, _ := img.At(x, y).RGBA()
			r8, g8, b8 := int(pr>>8), int(pg>>8), int(pb>>8)
			brightness := 0.2126*float64(r8) + 0.7152*float64(g8) + 0.0722*float64(b8)
			isBright := brightness > 160
			isNotGray := math.Abs(float64(r8)-float64(g8)) > 25 || math.Abs(float64(g8)-float64(b8)) > 25
			isNotWhite := !(r8 > 220 && g8 > 220 && b8 > 220)
			if isBright && isNotGray && isNotWhite {
				key := [3]int{r8, g8, b8}
				colorCount[key]++
			}
		}
	}

	maxCount := 0
	dr, dg, db := 200, 200, 200
	for c, count := range colorCount {
		if count > maxCount {
			maxCount = count
			dr, dg, db = c[0], c[1], c[2]
		}
	}
	return dr, dg, db
}

func renderSixel(img image.Image, colors int, dither bool) error {
	return EncodeSixelFrame(os.Stdout, img, colors, dither)
}

func renderKitty(img image.Image, widthChars, heightChars int) error {
	EncodeKittyFrame(os.Stdout, img, widthChars, heightChars)
	return nil
}

func EncodeSixelFrame(w io.Writer, img image.Image, colors int, dither bool) error {
	enc := &sixelEncoder{w: w, Colors: colors, Dither: dither, Workers: runtime.NumCPU()}
	return enc.Encode(img)
}

func EncodeKittyFrame(w io.Writer, img image.Image, c, r int) uint32 {
	bounds := img.Bounds()
	pixelW := bounds.Dx()
	pixelH := bounds.Dy()

	imageID := atomic.AddUint32(&kittyImageID, 1)

	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, rgba.Bounds(), img, bounds.Min, draw.Src)
	data := rgba.Pix

	var compressed bool
	var compData []byte
	if len(data) > 1024 {
		buf := kittyCompressPool.Get().(*bytes.Buffer)
		buf.Reset()
		zw := kittyZlibPool.Get().(*zlib.Writer)
		zw.Reset(buf)
		zw.Write(data)
		zw.Close()
		compData = buf.Bytes()
		compressed = true
		kittyZlibPool.Put(zw)
		kittyCompressPool.Put(buf)
	} else {
		compData = data
	}

	// Base64 编码到池化缓冲区，避免 string 分配
	encLen := base64.StdEncoding.EncodedLen(len(compData))
	base64Raw := kittyBase64Pool.Get().([]byte)
	if cap(base64Raw) < encLen {
		base64Raw = make([]byte, encLen)
	}
	base64Buf := base64Raw[:encLen]
	base64.StdEncoding.Encode(base64Buf, compData)

	// 控制头部
	if compressed {
		fmt.Fprintf(w, "\x1b_Ga=T,f=32,i=%d,s=%d,v=%d,c=%d,r=%d,q=2,o=z",
			imageID, pixelW, pixelH, c, r)
	} else {
		fmt.Fprintf(w, "\x1b_Ga=T,f=32,i=%d,s=%d,v=%d,c=%d,r=%d,q=2",
			imageID, pixelW, pixelH, c, r)
	}

	// 分块发送
	for i := 0; i < encLen; i += 4096 {
		end := i + 4096
		if end > encLen {
			end = encLen
		}
		chunk := base64Buf[i:end]

		if i == 0 {
			// 第一块：接在已有的控制头部后面
			if i+4096 < encLen {
				fmt.Fprintf(w, ",m=1;%s\x1b\\", chunk)
			} else {
				fmt.Fprintf(w, ";%s\x1b\\", chunk)
			}
		} else {
			// 后续块：需要完整的 \x1b_G 转义序列
			if i+4096 < encLen {
				fmt.Fprintf(w, "\x1b_Gm=1,q=2;%s\x1b\\", chunk)
			} else {
				fmt.Fprintf(w, "\x1b_Gm=0,q=2;%s\x1b\\", chunk)
			}
		}
	}

	kittyBase64Pool.Put(base64Raw[:0])
	return imageID
}

// EncodeKittyFrameRaw 直接从原始 RGBA 字节编码 Kitty 图像，跳过 image.RGBA 创建和 draw.Draw
// data 是 raw RGBA 字节 (4 bytes/pixel, R,G,B,A 顺序)
func EncodeKittyFrameRaw(w io.Writer, data []byte, pixelW, pixelH, c, r int) uint32 {
	imageID := atomic.AddUint32(&kittyImageID, 1)

	// 1. 压缩（使用池化 Buffer，避免分配）
	var compressed bool
	var compData []byte

	if len(data) > 1024 {
		buf := kittyCompressPool.Get().(*bytes.Buffer)
		buf.Reset()
		zw := kittyZlibPool.Get().(*zlib.Writer)
		zw.Reset(buf)
		_, _ = zw.Write(data)
		zw.Close()
		compData = buf.Bytes()
		compressed = true
		kittyZlibPool.Put(zw)
		kittyCompressPool.Put(buf)
	} else {
		compData = data
	}

	// 2. base64 编码到池化缓冲区（避免 EncodeToString 的 string 分配）
	encLen := base64.StdEncoding.EncodedLen(len(compData))
	base64Raw := kittyBase64Pool.Get().([]byte)
	if cap(base64Raw) < encLen {
		base64Raw = make([]byte, encLen)
	}
	base64Buf := base64Raw[:encLen]
	base64.StdEncoding.Encode(base64Buf, compData)

	// 3. 发送控制头部
	if compressed {
		fmt.Fprintf(w, "\x1b_Ga=T,f=32,i=%d,s=%d,v=%d,c=%d,r=%d,q=2,o=z",
			imageID, pixelW, pixelH, c, r)
	} else {
		fmt.Fprintf(w, "\x1b_Ga=T,f=32,i=%d,s=%d,v=%d,c=%d,r=%d,q=2",
			imageID, pixelW, pixelH, c, r)
	}

	// 4. 分块发送（每块 128KB，减少转义序列数量）
	const chunkSize = 131072
	for i := 0; i < encLen; i += chunkSize {
		end := i + chunkSize
		if end > encLen {
			end = encLen
		}
		chunk := base64Buf[i:end]

		if i == 0 {
			// 第一块：接在已有的控制头部后面
			if i+chunkSize < encLen {
				fmt.Fprintf(w, ",m=1;%s\x1b\\", chunk)
			} else {
				fmt.Fprintf(w, ";%s\x1b\\", chunk)
			}
		} else {
			// 后续块：需要完整的 \x1b_G 转义序列
			if i+chunkSize < encLen {
				fmt.Fprintf(w, "\x1b_Gm=1,q=2;%s\x1b\\", chunk)
			} else {
				fmt.Fprintf(w, "\x1b_Gm=0,q=2;%s\x1b\\", chunk)
			}
		}
	}

	// 5. 归还 base64 缓冲区
	kittyBase64Pool.Put(base64Raw[:0])

	return imageID
}

func DeleteKittyFrame(w io.Writer, id uint32) {
	fmt.Fprintf(w, "\x1b_Ga=d,d=i,i=%d\x1b\\", id)
}

// encodePNG is unused but kept for potential iTerm2 support; suppress unused warning.
var _ = encodePNG

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

func SupportsTrueColor() bool {
	ct := os.Getenv("COLORTERM")
	if ct == "truecolor" || ct == "24bit" {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Sixel 编码器 (从 BM 中提取并通用化)
// ---------------------------------------------------------------------------

type sixelEncoder struct {
	w       interface{ Write([]byte) (int, error) }
	Dither  bool
	Colors  int
	Workers int
}

type stripResult struct {
	startRow int
	sixelMap [][][]byte
}

func (e *sixelEncoder) Encode(img image.Image) error {
	nc := e.Colors
	if nc < 2 {
		nc = 255
	}

	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return nil
	}

	outBuf := bytes.NewBuffer(make([]byte, 0, 65536))
	outBuf.Write([]byte{0x1b, 0x50, 0x30, 0x3b, 0x30, 0x3b, 0x38, 0x71, 0x22, 0x31, 0x3b, 0x31})

	var paletted *image.Paletted
	if p, ok := img.(*image.Paletted); ok && len(p.Palette) <= nc {
		paletted = p
	} else {
		q := median.Quantizer(nc - 1)
		paletted = q.Paletted(img)
		if e.Dither {
			draw.FloydSteinberg.Draw(paletted, img.Bounds(), img, image.Point{})
		}
	}

	for i, c := range paletted.Palette {
		r, g, b, _ := c.RGBA()
		if i >= nc {
			break
		}
		fmt.Fprintf(outBuf, "#%d;2;%d;%d;%d", i+1, r*100/0xFFFF, g*100/0xFFFF, b*100/0xFFFF)
	}

	sixelRows := (height + 5) / 6
	workers := e.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > sixelRows {
		workers = sixelRows
	}
	rowsPerWorker := (sixelRows + workers - 1) / workers

	var wg sync.WaitGroup
	resultChan := make(chan stripResult, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			startRow := workerID * rowsPerWorker
			endRow := startRow + rowsPerWorker
			if endRow > sixelRows {
				endRow = sixelRows
			}
			if startRow >= endRow {
				return
			}
			e.processStrip(img, paletted, startRow, endRow, width, height, nc, resultChan)
		}(i)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	e.encodeSixelRows(outBuf, resultChan, sixelRows, width, len(paletted.Palette))

	outBuf.Write([]byte{0x1b, 0x5c})
	_, err := outBuf.WriteTo(&writerAdapter{w: e.w})
	return err
}

type writerAdapter struct {
	w interface{ Write([]byte) (int, error) }
}

func (a *writerAdapter) Write(p []byte) (int, error) {
	return a.w.Write(p)
}

func (e *sixelEncoder) processStrip(img image.Image, paletted *image.Paletted, startRow, endRow, width, totalHeight, nc int, resultChan chan<- stripResult) {
	sixelRows := endRow - startRow

	sixelMap := make([][][]byte, sixelRows)
	for z := 0; z < sixelRows; z++ {
		sixelMap[z] = make([][]byte, nc)
		for c := 0; c < nc; c++ {
			sixelMap[z][c] = make([]byte, width)
		}
	}

	startY := startRow * 6
	endY := endRow * 6
	if endY > totalHeight {
		endY = totalHeight
	}

	switch src := img.(type) {
	case *image.RGBA:
		pix := src.Pix
		stride := src.Stride
		for y := startY; y < endY; y++ {
			rowStart := y * stride
			z := y/6 - startRow
			bit := byte(y % 6)
			if y >= paletted.Bounds().Dy() {
				continue
			}
			for x := 0; x < width; x++ {
				if x >= paletted.Bounds().Dx() {
					continue
				}
				offset := rowStart + x*4
				if pix[offset+3] != 255 {
					continue
				}
				idx := int(paletted.ColorIndexAt(x, y))
				if idx < 0 || idx >= nc {
					continue
				}
				sixelMap[z][idx][x] |= 1 << bit
			}
		}
	default:
		imgBounds := img.Bounds()
		for y := startY; y < endY; y++ {
			z := y/6 - startRow
			bit := byte(y % 6)
			if y >= paletted.Bounds().Dy() {
				continue
			}
			for x := 0; x < width; x++ {
				if x >= paletted.Bounds().Dx() {
					continue
				}
				_, _, _, a := img.At(x+imgBounds.Min.X, y+imgBounds.Min.Y).RGBA()
				if a != 0xFFFF {
					continue
				}
				idx := int(paletted.ColorIndexAt(x, y))
				if idx < 0 || idx >= nc {
					continue
				}
				sixelMap[z][idx][x] |= 1 << bit
			}
		}
	}

	resultChan <- stripResult{
		startRow: startRow,
		sixelMap: sixelMap,
	}
}

func (e *sixelEncoder) encodeSixelRows(outBuf *bytes.Buffer, resultChan <-chan stripResult, totalRows, width, paletteSize int) {
	orderedResults := make([][][]byte, totalRows)

	for res := range resultChan {
		for i, colorData := range res.sixelMap {
			rowIdx := res.startRow + i
			if rowIdx < totalRows {
				orderedResults[rowIdx] = colorData
			}
		}
	}

	tempBuf := make([]byte, 0, 256)
	for z := 0; z < totalRows; z++ {
		if z > 0 {
			outBuf.WriteByte(0x2d)
		}
		if z >= len(orderedResults) || orderedResults[z] == nil {
			continue
		}
		colorData := orderedResults[z]

		for colorIdx := 0; colorIdx < paletteSize; colorIdx++ {
			sixelRow := colorData[colorIdx]

			hasData := false
			for x := 0; x < width; x++ {
				if sixelRow[x] != 0 {
					hasData = true
					break
				}
			}
			if !hasData {
				continue
			}

			outBuf.WriteByte(0x24)
			outBuf.WriteByte(0x23)

			colorNum := colorIdx + 1
			if colorNum >= 100 {
				outBuf.Write([]byte{
					byte(0x30 + colorNum/100),
					byte(0x30 + (colorNum%100)/10),
					byte(0x30 + colorNum%10),
				})
			} else if colorNum >= 10 {
				outBuf.Write([]byte{
					byte(0x30 + colorNum/10),
					byte(0x30 + colorNum%10),
				})
			} else {
				outBuf.WriteByte(byte(0x30 + colorNum))
			}

			var lastCh byte
			runCount := 0
			for x := 0; x <= width; x++ {
				var ch byte
				if x < width {
					ch = sixelRow[x]
				} else {
					ch = 0xff
				}
				if ch != lastCh || runCount == 255 {
					if runCount > 0 {
						sixelChar := lastCh + 63
						tempBuf = tempBuf[:0]
						if runCount > 1 {
							tempBuf = append(tempBuf, 0x21)
							if runCount >= 100 {
								tempBuf = append(tempBuf,
									byte(0x30+runCount/100),
									byte(0x30+(runCount%100)/10),
									byte(0x30+runCount%10),
								)
							} else if runCount >= 10 {
								tempBuf = append(tempBuf,
									byte(0x30+runCount/10),
									byte(0x30+runCount%10),
								)
							} else {
								tempBuf = append(tempBuf, byte(0x30+runCount))
							}
						}
						tempBuf = append(tempBuf, sixelChar)
						outBuf.Write(tempBuf)
					}
					lastCh = ch
					runCount = 1
				} else {
					runCount++
				}
			}
		}
	}
}

func IsInTmuxOrZellij() bool {
	if os.Getenv("TMUX") != "" || os.Getenv("TMUX_PANE") != "" {
		return true
	}
	if os.Getenv("ZELLIJ") != "" || os.Getenv("ZELLIJ_SESSION_NAME") != "" {
		return true
	}
	return false
}

func IsKittyAvailable() bool {
	return detectProtocol() == ProtocolKitty
}

func IsSixelAvailable() bool {
	if term := os.Getenv("TERM_PROGRAM"); term == "foot" || term == "contour" || term == "mintty" || term == "RLogin" {
		return true
	}

	term := strings.ToLower(os.Getenv("TERM"))
	for _, t := range []string{"foot", "mlterm", "contour", "xterm-sixel", "yaft-256color"} {
		if strings.Contains(term, t) {
			return true
		}
	}

	return queryDECPrivateMode(1)
}

func DetectCapableProtocol() (Protocol, bool) {
	if IsInTmuxOrZellij() {
		return 0, false
	}
	if IsKittyAvailable() {
		return ProtocolKitty, true
	}
	if IsSixelAvailable() {
		return ProtocolSixel, true
	}
	return 0, false
}

func queryDECPrivateMode(mode int) bool {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return false
	}
	defer term.Restore(fd, old)

	fmt.Printf("\033[?%d$p", mode)

	done := make(chan bool, 1)
	go func() {
		var buf []byte
		b := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(b)
			if err != nil || n == 0 {
				done <- false
				return
			}
			buf = append(buf, b[0])
			if b[0] == 'y' {
				s := string(buf)
				si := strings.IndexByte(s, ';')
				di := strings.IndexByte(s, '$')
				if si >= 0 && di == si+2 {
					done <- s[si+1] != '0'
					return
				}
				done <- false
				return
			}
			if len(buf) > 20 {
				done <- false
				return
			}
		}
	}()

	select {
	case ok := <-done:
		return ok
	case <-time.After(100 * time.Millisecond):
		return false
	}
}
