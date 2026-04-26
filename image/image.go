package image

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
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
	return encodeSixel(w, img, colors, dither)
}

// ---- 快速均匀量化（8×8×4 = 256 色，纯位运算，无 LUT，O(n)） ----
var uniform256Palette []color.Color
var uniform256Once sync.Once

func initUniform256() {
	uniform256Palette = make([]color.Color, 256)
	for ri := 0; ri < 8; ri++ {
		for gi := 0; gi < 8; gi++ {
			for bi := 0; bi < 4; bi++ {
				idx := ri<<5 | gi<<2 | bi
				uniform256Palette[idx] = color.RGBA{
					uint8(ri * 255 / 7),
					uint8(gi * 255 / 7),
					uint8(bi * 255 / 3),
					255,
				}
			}
		}
	}
}

// Bayer 4×4 有序抖动矩阵
var bayer4 = [16]uint8{
	0, 8, 2, 10,
	12, 4, 14, 6,
	3, 11, 1, 9,
	15, 7, 13, 5,
}

// quantizeUniform 用均匀 8×8×4 色块 + Bayer 有序抖动量化 raw RGBA
// 抖动将量化误差分散到邻近像素，视觉上消除色带
// idx = (R_idx<<5) | (G_idx<<2) | B_idx — 纯位运算，每像素 O(1)
func quantizeUniform(data []byte, width, height int) *image.Paletted {
	uniform256Once.Do(initUniform256)
	pi := image.NewPaletted(image.Rect(0, 0, width, height), uniform256Palette)
	dst := pi.Pix
	si := 0
	di := 0
	for y := 0; y < height; y++ {
		yb := y & 3
		for x := 0; x < width; x++ {
			r := int(data[si])
			g := int(data[si+1])
			b := int(data[si+2])
			si += 4

			t := int(bayer4[(yb<<2)|(x&3)])
			// 将 Bayer 阈值 0..15 映射到 ±15 (R/G) 和 ±30 (B)
			// 加到原始值上使邻近像素落在不同色块，消除硬边界
			ri := (r + t*2 - 15) >> 5
			gi := (g + t*2 - 15) >> 5
			bi := (b + t*4 - 30) >> 6

			if ri < 0 {
				ri = 0
			} else if ri > 7 {
				ri = 7
			}
			if gi < 0 {
				gi = 0
			} else if gi > 7 {
				gi = 7
			}
			if bi < 0 {
				bi = 0
			} else if bi > 3 {
				bi = 3
			}

			dst[di] = uint8(ri<<5 | gi<<2 | bi)
			di++
		}
	}
	return pi
}

// EncodeSixelFrameRaw 直接从 raw RGBA 字节编码 Sixel
// 使用均匀量化（O(n)，纯位运算）+ 并行 strip 编码
func EncodeSixelFrameRaw(w io.Writer, data []byte, width, height int, colors int, dither bool) error {
	if width == 0 || height == 0 {
		return nil
	}
	paletted := quantizeUniform(data, width, height)
	return encodePaletted(w, paletted, width, height, 256)
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
// kittyRGBPool 复用 RGBA→RGB 转换的输出缓冲区
var kittyRGBPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 512*1024) // 512KB 初始容量
	},
}

// EncodeKittyFrameRaw 直接从原始 RGBA 字节编码 Kitty 图像
// data 是 raw RGBA 字节 (4 bytes/pixel, R,G,B,A 顺序)
// 针对视频优化：自动剥离 alpha 通道使用 RGB(f=24)，不压缩
func EncodeKittyFrameRaw(w io.Writer, data []byte, pixelW, pixelH, c, r int) uint32 {
	imageID := atomic.AddUint32(&kittyImageID, 1)

	// 1. RGBA → RGB：剥离 alpha 通道，省 25% 数据量
	rgbLen := pixelW * pixelH * 3
	rgbRaw := kittyRGBPool.Get().([]byte)
	if cap(rgbRaw) < rgbLen {
		rgbRaw = make([]byte, rgbLen)
	}
	rgbBuf := rgbRaw[:rgbLen]

	// 批量拷贝 RGB，跳过 A（优化：8 像素批量处理）
	// 一次性处理 8 像素，减少循环开销
	blocks := len(data) / 32
	j := 0
	for b := 0; b < blocks; b++ {
		s := b * 32
		// 展开循环：每次处理 8 像素
		rgbBuf[j] = data[s]; rgbBuf[j+1] = data[s+1]; rgbBuf[j+2] = data[s+2]
		rgbBuf[j+3] = data[s+4]; rgbBuf[j+4] = data[s+5]; rgbBuf[j+5] = data[s+6]
		rgbBuf[j+6] = data[s+8]; rgbBuf[j+7] = data[s+9]; rgbBuf[j+8] = data[s+10]
		rgbBuf[j+9] = data[s+12]; rgbBuf[j+10] = data[s+13]; rgbBuf[j+11] = data[s+14]
		rgbBuf[j+12] = data[s+16]; rgbBuf[j+13] = data[s+17]; rgbBuf[j+14] = data[s+18]
		rgbBuf[j+15] = data[s+20]; rgbBuf[j+16] = data[s+21]; rgbBuf[j+17] = data[s+22]
		rgbBuf[j+18] = data[s+24]; rgbBuf[j+19] = data[s+25]; rgbBuf[j+20] = data[s+26]
		rgbBuf[j+21] = data[s+28]; rgbBuf[j+22] = data[s+29]; rgbBuf[j+23] = data[s+30]
		j += 24
	}
	// 处理剩余的像素
	remain := len(data) % 32
	for i := 0; i < remain; i += 4 {
		rgbBuf[j] = data[blocks*32+i]
		rgbBuf[j+1] = data[blocks*32+i+1]
		rgbBuf[j+2] = data[blocks*32+i+2]
		j += 3
	}

	// 2. base64 编码到池化缓冲区（避免 EncodeToString 的 string 分配）
	encLen := base64.StdEncoding.EncodedLen(rgbLen)
	base64Raw := kittyBase64Pool.Get().([]byte)
	if cap(base64Raw) < encLen {
		base64Raw = make([]byte, encLen)
	}
	base64Buf := base64Raw[:encLen]
	base64.StdEncoding.Encode(base64Buf, rgbBuf)

	// 归还 RGB 缓冲区
	kittyRGBPool.Put(rgbRaw[:0])

	// 3. 发送控制头部（f=24 = RGB, 不压缩）- 使用 bytes.Buffer 直接写入
	bw, ok := w.(*bytes.Buffer)
	if ok {
		// 直接写入 buffer，避免 fmt.Fprintf 开销
		bw.WriteString("\x1b_Ga=T,f=24,i=")
		writeUint32(bw, imageID)
		bw.WriteString(",s=")
		writeUint32(bw, uint32(pixelW))
		bw.WriteString(",v=")
		writeUint32(bw, uint32(pixelH))
		bw.WriteString(",c=")
		writeUint32(bw, uint32(c))
		bw.WriteString(",r=")
		writeUint32(bw, uint32(r))
		bw.WriteString(",q=2")
	} else {
		fmt.Fprintf(w, "\x1b_Ga=T,f=24,i=%d,s=%d,v=%d,c=%d,r=%d,q=2",
			imageID, pixelW, pixelH, c, r)
	}

	// 4. 分块发送（每块 512KB，减少分块数量）
	const chunkSize = 524288
	if ok {
		for i := 0; i < encLen; i += chunkSize {
			end := i + chunkSize
			if end > encLen {
				end = encLen
			}
			chunk := base64Buf[i:end]
			if i == 0 {
				if i+chunkSize < encLen {
					bw.WriteString(",m=1;")
				} else {
					bw.WriteString(";")
				}
				bw.Write(chunk)
				bw.WriteString("\x1b\\")
			} else {
				bw.WriteString("\x1b_Gm=")
				if i+chunkSize < encLen {
					bw.WriteByte('1')
				} else {
					bw.WriteByte('0')
				}
				bw.WriteString(",q=2;")
				bw.Write(chunk)
				bw.WriteString("\x1b\\")
			}
		}
	} else {
		for i := 0; i < encLen; i += chunkSize {
			end := i + chunkSize
			if end > encLen {
				end = encLen
			}
			chunk := base64Buf[i:end]
			if i == 0 {
				if i+chunkSize < encLen {
					fmt.Fprintf(w, ",m=1;%s\x1b\\", chunk)
				} else {
					fmt.Fprintf(w, ";%s\x1b\\", chunk)
				}
			} else {
				if i+chunkSize < encLen {
					fmt.Fprintf(w, "\x1b_Gm=1,q=2;%s\x1b\\", chunk)
				} else {
					fmt.Fprintf(w, "\x1b_Gm=0,q=2;%s\x1b\\", chunk)
				}
			}
		}
	}

	// 5. 归还 base64 缓冲区
	kittyBase64Pool.Put(base64Raw[:0])

	return imageID
}

// writeUint32 高效写入无符号整数到 buffer
func writeUint32(b *bytes.Buffer, n uint32) {
	if n >= 1000000000 {
		b.WriteByte(byte('0' + n/1000000000%10))
		b.WriteByte(byte('0' + n/100000000%10))
		b.WriteByte(byte('0' + n/10000000%10))
		b.WriteByte(byte('0' + n/1000000%10))
		b.WriteByte(byte('0' + n/100000%10))
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 100000000 {
		b.WriteByte(byte('0' + n/100000000%10))
		b.WriteByte(byte('0' + n/10000000%10))
		b.WriteByte(byte('0' + n/1000000%10))
		b.WriteByte(byte('0' + n/100000%10))
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 10000000 {
		b.WriteByte(byte('0' + n/10000000%10))
		b.WriteByte(byte('0' + n/1000000%10))
		b.WriteByte(byte('0' + n/100000%10))
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 1000000 {
		b.WriteByte(byte('0' + n/1000000%10))
		b.WriteByte(byte('0' + n/100000%10))
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 100000 {
		b.WriteByte(byte('0' + n/100000%10))
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 10000 {
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 1000 {
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 100 {
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 10 {
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else {
		b.WriteByte(byte('0' + n))
	}
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

// ---------------------------------------------------------------------------
// Sixel 编码器 — 并行 strip 处理 + 流式 RLE 编码
// 每个 Worker 复用 flat [colors × width]byte 缓冲区（≈326KB for 720p × 255色）
// 通过通道回传结果，主 goroutine 保序编码，峰值内存 O(workers × colors × width)
// ---------------------------------------------------------------------------

// sixelStripState 复用 strip 编码的临时缓冲区
var sixelStripStatePool = sync.Pool{
	New: func() interface{} {
		return &sixelStripState{
			dirty: make([]int, 0, 64),
		}
	},
}

type sixelStripState struct {
	buf   []byte  // flat [colors * width]byte  bitmap
	epoch []uint8 // [colors]uint8 每个颜色的 epoch 标记
	dirty []int   // 当前 strip 中被写过的颜色索引
}

// stripJob 表示一个待处理的 Sixel 行
// yStart/yEnd 是像素行范围（yStart ∈ [0, height)，最多 6 行）
type stripJob struct {
	sixelRow int
	yStart   int
	yEnd     int
}

// stripResult 是 Worker 处理完一个 strip 后的结果
type stripResult struct {
	sixelRow int
	state    *sixelStripState // 包含 buf 和 dirty 列表，编码后归还到池
}

func encodeSixel(w io.Writer, img image.Image, colors int, dither bool) error {
	nc := colors
	if nc < 2 {
		nc = 255
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return nil
	}
	var paletted *image.Paletted
	if p, ok := img.(*image.Paletted); ok && len(p.Palette) <= nc {
		paletted = p
	} else {
		q := median.Quantizer(nc - 1)
		paletted = q.Paletted(img)
		if dither {
			draw.FloydSteinberg.Draw(paletted, img.Bounds(), img, image.Point{})
		}
	}
	return encodePaletted(w, paletted, width, height, nc)
}

// encodePaletted 编码已量化的 Paletted 图像为 Sixel（并行 strip 处理 + 流式 RLE）
func encodePaletted(w io.Writer, paletted *image.Paletted, width, height, nc int) error {
	pix := paletted.Pix
	stride := paletted.Stride

	paletteSize := len(paletted.Palette)
	if paletteSize > nc {
		paletteSize = nc
	}

	// ---- 输出 Sixel 头部 ----
	estSize := width * height / 2
	if estSize < 65536 {
		estSize = 65536
	}
	outBuf := bytes.NewBuffer(make([]byte, 0, estSize))
	outBuf.Write([]byte{0x1b, 0x50, 0x30, 0x3b, 0x30, 0x3b, 0x38, 0x71, 0x22, 0x31, 0x3b, 0x31})
	for i := 0; i < paletteSize; i++ {
		r, g, b, _ := paletted.Palette[i].RGBA()
		fmt.Fprintf(outBuf, "#%d;2;%d;%d;%d", i+1, r*100/0xFFFF, g*100/0xFFFF, b*100/0xFFFF)
	}

	// ---- 并行 strip 处理（交错发送/接收，避免死锁） ----
	totalSixelRows := (height + 5) / 6
	workers := runtime.NumCPU()
	if workers > totalSixelRows {
		workers = totalSixelRows
	}
	if workers < 1 {
		workers = 1
	}

	// 使用足够大的缓冲避免死锁：主 goroutine 先发送一批任务，
	// 然后交错执行「发一个任务 / 收一个结果」，保证 resultCh 不会填满
	jobCh := make(chan stripJob, workers)
	resultCh := make(chan stripResult, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				st := sixelStripStatePool.Get().(*sixelStripState)
				stripCap := nc * width
				if cap(st.buf) < stripCap {
					st.buf = make([]byte, stripCap)
				}
				st.buf = st.buf[:stripCap]
				if cap(st.epoch) < nc {
					st.epoch = make([]uint8, nc)
				} else {
					st.epoch = st.epoch[:nc]
				}
				clear(st.epoch)
				dirty := st.dirty[:0]

				for dy := job.yStart; dy < job.yEnd; dy++ {
					bit := byte(1 << (dy - job.yStart))
					rowOffset := dy * stride
					for x := 0; x < width; x++ {
						idx := int(pix[rowOffset+x])
						if idx >= nc {
							continue
						}
						if st.epoch[idx] != 1 {
							st.epoch[idx] = 1
							dirty = append(dirty, idx)
							clear(st.buf[idx*width : (idx+1)*width])
						}
						st.buf[idx*width+x] |= bit
					}
				}

				st.dirty = dirty
				resultCh <- stripResult{sixelRow: job.sixelRow, state: st}
			}
		}()
	}

	// 先发送一批任务（保证 Worker 初始不会空闲）
	jobsSent := 0
	for ; jobsSent < workers && jobsSent < totalSixelRows; jobsSent++ {
		yStart := jobsSent * 6
		yEnd := yStart + 6
		if yEnd > height {
			yEnd = height
		}
		jobCh <- stripJob{sixelRow: jobsSent, yStart: yStart, yEnd: yEnd}
	}

	// 交错执行：发一个任务 → 收一个结果 → 编码 → 继续
	makeJob := func(sixelRow int) stripJob {
		yStart := sixelRow * 6
		yEnd := yStart + 6
		if yEnd > height {
			yEnd = height
		}
		return stripJob{sixelRow: sixelRow, yStart: yStart, yEnd: yEnd}
	}

	nextRow := 0
	pending := make(map[int]*sixelStripState)
	recvCount := 0

	for recvCount < totalSixelRows {
		if jobsSent < totalSixelRows {
			// 还有任务待发：select 确保不会同时阻塞在 jobCh 和 resultCh
			select {
			case jobCh <- makeJob(jobsSent):
				jobsSent++
			case res := <-resultCh:
				recvCount++
				processStripResult(outBuf, res, &nextRow, pending, width)
			}
		} else {
			// 所有任务已发完，只收结果
			res := <-resultCh
			recvCount++
			processStripResult(outBuf, res, &nextRow, pending, width)
		}
	}

	close(jobCh)
	wg.Wait()

	// Sixel 终止符
	outBuf.Write([]byte{0x1b, 0x5c})

	_, err := outBuf.WriteTo(w)
	return err
}

// processStripResult 保序处理一个 strip 结果：如果顺序正确立即编码，否则暂存
func processStripResult(outBuf *bytes.Buffer, res stripResult, nextRow *int, pending map[int]*sixelStripState, width int) {
	if res.sixelRow == *nextRow {
		encodeStrip(outBuf, res.state, res.sixelRow, width)
		*nextRow++
		for {
			if st, ok := pending[*nextRow]; ok {
				encodeStrip(outBuf, st, *nextRow, width)
				delete(pending, *nextRow)
				*nextRow++
			} else {
				break
			}
		}
	} else {
		pending[res.sixelRow] = res.state
	}
}

// encodeStrip RLE 编码一个 strip 的所有颜色行到 outBuf，然后归还 state 到池
func encodeStrip(outBuf *bytes.Buffer, st *sixelStripState, sixelRow, width int) {
	if st == nil {
		return
	}

	if sixelRow > 0 {
		outBuf.WriteByte(0x2d) // '-' 分隔 strip
	}

	for _, c := range st.dirty {
		base := c * width
		row := st.buf[base : base+width]

		outBuf.WriteByte(0x24) // '$'
		outBuf.WriteByte(0x23) // '#'
		writeSixelNum(outBuf, c+1)

		var lastCh byte
		runCount := 0
		for x := 0; x <= width; x++ {
			var ch byte
			if x < width {
				ch = row[x]
			} else {
				ch = 0xff
			}
			if ch != lastCh || runCount == 255 {
				if runCount > 0 {
					sixelChar := lastCh + 63
					if runCount > 1 {
						outBuf.WriteByte(0x21) // '!'
						writeSixelNum(outBuf, runCount)
					}
					outBuf.WriteByte(sixelChar)
				}
				lastCh = ch
				runCount = 1
			} else {
				runCount++
			}
		}
	}

	st.dirty = st.dirty[:0]
	sixelStripStatePool.Put(st)
}

// writeSixelNum 高效写入 Sixel 数字（颜色编号或游程长度）到 buffer
func writeSixelNum(b *bytes.Buffer, n int) {
	if n >= 100 {
		b.Write([]byte{
			byte(0x30 + n/100),
			byte(0x30 + (n%100)/10),
			byte(0x30 + n%10),
		})
	} else if n >= 10 {
		b.Write([]byte{
			byte(0x30 + n/10),
			byte(0x30 + n%10),
		})
	} else {
		b.WriteByte(byte(0x30 + n))
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
