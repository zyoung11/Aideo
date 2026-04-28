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
	Left   float64
	Top    float64
	Width  float64
	Height float64
}

type DisplayConfig struct {
	Img                 *image.RGBA
	Placement           *Placement
	KeepAspectRatio     bool
	PadCol              int
	PadRow              int
	SixelColors         int
	SixelDither         bool
	ForceProtocol       Protocol
	DisableColorAnalysis bool
	CellW               int
	CellH               int
}

type DisplayResult struct {
	Placeholder Placeholder
	Dominant    ColorRGB
}

var kittyImageID uint32 = uint32(os.Getpid()<<16) + uint32(time.Now().UnixMicro()&0xFFFF)

var kittyZlibPool = sync.Pool{
	New: func() interface{} {
		w, _ := zlib.NewWriterLevel(nil, zlib.BestSpeed)
		return w
	},
}

var kittyCompressPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

var kittyBase64Pool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 256*1024)
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

		targetPixelW := targetCols * cellW
		targetPixelH := targetRows * cellH

		var scaled image.Image
		if cfg.KeepAspectRatio {
			scaled = resize.Thumbnail(uint(targetPixelW), uint(targetPixelH), cfg.Img, resize.Lanczos3)
		} else {
			scaled = resize.Resize(uint(targetPixelW), uint(targetPixelH), cfg.Img, resize.Lanczos3)
		}

		finalW, finalH := scaled.Bounds().Dx(), scaled.Bounds().Dy()

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
			bounds := scaled.Bounds()
			w, h := bounds.Dx(), bounds.Dy()
			rgba := image.NewRGBA(bounds)
			draw.Draw(rgba, bounds, scaled, bounds.Min, draw.Src)
			err = EncodeSixelRGBA(os.Stdout, rgba.Pix, w, h, 256)
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

var bayer4x4 = [16]int8{0, 8, 2, 10, 12, 4, 14, 6, 3, 11, 1, 9, 15, 7, 13, 5}

func makeDither(levels int) [16]int8 {
	step := 256 / (levels - 1)
	var d [16]int8
	for i, v := range bayer4x4 {
		d[i] = int8((int(v) - 8) * step / 16)
	}
	return d
}

type sixelPalette struct {
	nc     int
	isGray bool
	colors []color.Color
	lutR   [16][256]uint8
	lutG   [16][256]uint8
	lutB   [16][256]uint8
	dither [16]int8
}

func newSixelPalette(rBits, gBits, bBits int) *sixelPalette {
	rLevels := 1 << rBits
	gLevels := 1 << gBits
	bLevels := 1 << bBits
	nc := rLevels * gLevels * bLevels
	gShift := bBits
	rShift := gBits + bBits

	p := &sixelPalette{nc: nc, colors: make([]color.Color, nc)}
	for ri := 0; ri < rLevels; ri++ {
		for gi := 0; gi < gLevels; gi++ {
			for bi := 0; bi < bLevels; bi++ {
				p.colors[ri<<rShift|gi<<gShift|bi] = color.RGBA{
					uint8(ri * 255 / (rLevels - 1)),
					uint8(gi * 255 / (gLevels - 1)),
					uint8(bi * 255 / (bLevels - 1)),
					255,
				}
			}
		}
	}

	dR := makeDither(rLevels)
	dG := makeDither(gLevels)
	dB := makeDither(bLevels)

	for b := 0; b < 16; b++ {
		for v := 0; v < 256; v++ {
			s := v + int(dR[b])
			if s < 0 {
				s = 0
			} else if s > 255 {
				s = 255
			}
			p.lutR[b][v] = uint8(s>>(8-rBits)) << rShift

			s = v + int(dG[b])
			if s < 0 {
				s = 0
			} else if s > 255 {
				s = 255
			}
			p.lutG[b][v] = uint8(s>>(8-gBits)) << gShift

			s = v + int(dB[b])
			if s < 0 {
				s = 0
			} else if s > 255 {
				s = 255
			}
			p.lutB[b][v] = uint8(s >> (8 - bBits))
		}
	}
	return p
}

func newGrayPalette(levels int) *sixelPalette {
	p := &sixelPalette{nc: levels, isGray: true, colors: make([]color.Color, levels)}
	for i := 0; i < levels; i++ {
		v := uint8(i * 255 / (levels - 1))
		p.colors[i] = color.RGBA{v, v, v, 255}
	}
	step := 256 / levels
	for b := 0; b < 16; b++ {
		p.dither[b] = int8((int(bayer4x4[b]) - 8) * step / 16)
	}
	return p
}

var paletteLevels = []int{2, 8, 16, 32, 64, 128, 256}
var paletteBits = [][3]int{{0, 0, 0}, {1, 1, 1}, {2, 1, 1}, {2, 2, 1}, {2, 2, 2}, {3, 2, 2}, {3, 3, 2}}
var paletteCache = make(map[int]*sixelPalette)
var paletteCacheMu sync.Mutex

func getSixelPalette(nc int) *sixelPalette {
	paletteCacheMu.Lock()
	defer paletteCacheMu.Unlock()
	if p, ok := paletteCache[nc]; ok {
		return p
	}
	if nc == 2 {
		p := newGrayPalette(2)
		paletteCache[nc] = p
		return p
	}
	for i, lvl := range paletteLevels {
		if lvl == nc {
			bits := paletteBits[i]
			p := newSixelPalette(bits[0], bits[1], bits[2])
			paletteCache[nc] = p
			return p
		}
	}
	return nil
}

func nearestPaletteLevel(nc int) int {
	for _, lvl := range paletteLevels {
		if lvl >= nc {
			return lvl
		}
	}
	return paletteLevels[len(paletteLevels)-1]
}

func EncodeSixelRGBA(w io.Writer, data []byte, width, height, colors int) error {
	if width == 0 || height == 0 {
		return nil
	}
	nc := nearestPaletteLevel(colors)
	pal := getSixelPalette(nc)
	return encodeSixelFromRGBA(w, data, width, height, pal, nil)
}

func EncodeSixelFrameRaw(w io.Writer, data []byte, width, height int, colors int, dither bool, cache *SixelFrameCache) error {
	if width == 0 || height == 0 {
		return nil
	}
	nc := nearestPaletteLevel(colors)
	pal := getSixelPalette(nc)
	return encodeSixelFromRGBA(w, data, width, height, pal, cache)
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

	encLen := base64.StdEncoding.EncodedLen(len(compData))
	base64Raw := kittyBase64Pool.Get().([]byte)
	if cap(base64Raw) < encLen {
		base64Raw = make([]byte, encLen)
	}
	base64Buf := base64Raw[:encLen]
	base64.StdEncoding.Encode(base64Buf, compData)

	if compressed {
		fmt.Fprintf(w, "\x1b_Ga=T,f=32,i=%d,s=%d,v=%d,c=%d,r=%d,q=2,o=z",
			imageID, pixelW, pixelH, c, r)
	} else {
		fmt.Fprintf(w, "\x1b_Ga=T,f=32,i=%d,s=%d,v=%d,c=%d,r=%d,q=2",
			imageID, pixelW, pixelH, c, r)
	}

	for i := 0; i < encLen; i += 4096 {
		end := i + 4096
		if end > encLen {
			end = encLen
		}
		chunk := base64Buf[i:end]

		if i == 0 {
			if i+4096 < encLen {
				fmt.Fprintf(w, ",m=1;%s\x1b\\", chunk)
			} else {
				fmt.Fprintf(w, ";%s\x1b\\", chunk)
			}
		} else {
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

var kittyRGBPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 512*1024)
	},
}

var sixelRLEBufPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 4096))
	},
}

func EncodeKittyFrameRaw(w io.Writer, data []byte, pixelW, pixelH, c, r int) uint32 {
	imageID := atomic.AddUint32(&kittyImageID, 1)

	rgbLen := pixelW * pixelH * 3
	rgbRaw := kittyRGBPool.Get().([]byte)
	if cap(rgbRaw) < rgbLen {
		rgbRaw = make([]byte, rgbLen)
	}
	rgbBuf := rgbRaw[:rgbLen]

	blocks := len(data) / 32
	j := 0
	for b := 0; b < blocks; b++ {
		s := b * 32
		rgbBuf[j] = data[s]
		rgbBuf[j+1] = data[s+1]
		rgbBuf[j+2] = data[s+2]
		rgbBuf[j+3] = data[s+4]
		rgbBuf[j+4] = data[s+5]
		rgbBuf[j+5] = data[s+6]
		rgbBuf[j+6] = data[s+8]
		rgbBuf[j+7] = data[s+9]
		rgbBuf[j+8] = data[s+10]
		rgbBuf[j+9] = data[s+12]
		rgbBuf[j+10] = data[s+13]
		rgbBuf[j+11] = data[s+14]
		rgbBuf[j+12] = data[s+16]
		rgbBuf[j+13] = data[s+17]
		rgbBuf[j+14] = data[s+18]
		rgbBuf[j+15] = data[s+20]
		rgbBuf[j+16] = data[s+21]
		rgbBuf[j+17] = data[s+22]
		rgbBuf[j+18] = data[s+24]
		rgbBuf[j+19] = data[s+25]
		rgbBuf[j+20] = data[s+26]
		rgbBuf[j+21] = data[s+28]
		rgbBuf[j+22] = data[s+29]
		rgbBuf[j+23] = data[s+30]
		j += 24
	}
	remain := len(data) % 32
	for i := 0; i < remain; i += 4 {
		rgbBuf[j] = data[blocks*32+i]
		rgbBuf[j+1] = data[blocks*32+i+1]
		rgbBuf[j+2] = data[blocks*32+i+2]
		j += 3
	}

	encLen := base64.StdEncoding.EncodedLen(rgbLen)
	base64Raw := kittyBase64Pool.Get().([]byte)
	if cap(base64Raw) < encLen {
		base64Raw = make([]byte, encLen)
	}
	base64Buf := base64Raw[:encLen]
	base64.StdEncoding.Encode(base64Buf, rgbBuf)

	kittyRGBPool.Put(rgbRaw[:0])

	bw, ok := w.(*bytes.Buffer)
	if ok {
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

	kittyBase64Pool.Put(base64Raw[:0])
	return imageID
}

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

var sixelStripStatePool = sync.Pool{
	New: func() interface{} {
		return &sixelStripState{
			dirty: make([]int, 0, 64),
		}
	},
}

type sixelStripState struct {
	buf       []byte
	seen      []uint16
	epoch     uint16
	dirty     []int
	pixelHash uint64
}

type SixelFrameCache struct {
	strips []sixelCachedStrip
	mu     sync.Mutex
}

type sixelCachedStrip struct {
	rleData   []byte
	pixelHash uint64
}

func NewSixelFrameCache(totalStrips int) *SixelFrameCache {
	return &SixelFrameCache{
		strips: make([]sixelCachedStrip, totalStrips),
	}
}

func stripPixelHash(pix []uint8, stride int, yStart, yEnd, width int) uint64 {
	h := uint64(14695981039346656037)
	for y := yStart; y < yEnd; y++ {
		row := pix[y*stride : y*stride+width]
		for _, p := range row {
			h ^= uint64(p)
			h *= 1099511628211
		}
	}
	return h
}

func stripPixelHashRGBA(data []byte, width int, yStart, yEnd int) uint64 {
	h := uint64(14695981039346656037)
	rowLen := width * 4
	for y := yStart; y < yEnd; y++ {
		row := data[y*rowLen : y*rowLen+rowLen]
		for _, b := range row {
			h ^= uint64(b)
			h *= 1099511628211
		}
	}
	return h
}

type pendingItem struct {
	state   *sixelStripState
	rleData []byte
}

type stripJob struct {
	sixelRow int
	yStart   int
	yEnd     int
}

type stripResult struct {
	sixelRow int
	state    *sixelStripState
	rleData  []byte
}

func encodeSixelFromRGBA(w io.Writer, data []byte, width, height int, pal *sixelPalette, cache *SixelFrameCache) error {
	nc := pal.nc
	totalSixelRows := (height + 5) / 6
	workers := runtime.NumCPU()
	if workers > totalSixelRows {
		workers = totalSixelRows
	}
	if workers < 1 {
		workers = 1
	}

	estSize := width * height / 2
	if estSize < 65536 {
		estSize = 65536
	}
	outBuf := bytes.NewBuffer(make([]byte, 0, estSize))
	outBuf.Write([]byte{0x1b, 0x50, 0x30, 0x3b, 0x30, 0x3b, 0x38, 0x71, 0x22})
	writeSixelNum(outBuf, width)
	outBuf.WriteByte(';')
	writeSixelNum(outBuf, height)
	outBuf.Write([]byte{0x3b, 0x31, 0x3b, 0x31})
	for i := 0; i < nc; i++ {
		r, g, b, _ := pal.colors[i].RGBA()
		outBuf.WriteByte('#')
		writeSixelNum(outBuf, i+1)
		outBuf.WriteString(";2;")
		writeSixelNum(outBuf, int(r*100/0xFFFF))
		outBuf.WriteByte(';')
		writeSixelNum(outBuf, int(g*100/0xFFFF))
		outBuf.WriteByte(';')
		writeSixelNum(outBuf, int(b*100/0xFFFF))
	}

	jobCh := make(chan stripJob, workers)
	resultCh := make(chan stripResult, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				nRows := job.yEnd - job.yStart
				pHash := stripPixelHashRGBA(data, width, job.yStart, job.yEnd)

				if cache != nil {
					cache.mu.Lock()
					c := &cache.strips[job.sixelRow]
					if c.pixelHash == pHash && len(c.rleData) > 0 {
						rleCopy := make([]byte, len(c.rleData))
						copy(rleCopy, c.rleData)
						cache.mu.Unlock()
						resultCh <- stripResult{sixelRow: job.sixelRow, rleData: rleCopy}
						continue
					}
					cache.mu.Unlock()
				}

				st := sixelStripStatePool.Get().(*sixelStripState)
				st.pixelHash = pHash
				stripCap := nc * width
				if cap(st.buf) < stripCap {
					st.buf = make([]byte, stripCap)
				}
				st.buf = st.buf[:stripCap]
				if cap(st.seen) < nc {
					st.seen = make([]uint16, nc)
				} else {
					st.seen = st.seen[:nc]
				}
				st.epoch++
				if st.epoch == 0 {
					clear(st.seen)
					st.epoch = 1
				}
				clear(st.buf[:stripCap])
				dirty := st.dirty[:0]

				rowOffset := job.yStart * width * 4
			if pal.isGray {
				shift := 8
				for n := pal.nc >> 1; n > 0; n >>= 1 {
					shift--
				}
				for dy := 0; dy < nRows; dy++ {
					yb4 := ((job.yStart + dy) & 3) << 2
					bit := byte(1 << dy)
					pi := rowOffset
					for x := 0; x < width; x++ {
						bayerIdx := yb4 | (x & 3)
						gray := (6966*int(data[pi]) + 23436*int(data[pi+1]) + 2366*int(data[pi+2])) >> 15
						gray += int(pal.dither[bayerIdx])
						if gray < 0 {
							gray = 0
						} else if gray > 255 {
							gray = 255
						}
						ci := gray >> shift
						if st.seen[ci] != st.epoch {
							st.seen[ci] = st.epoch
							dirty = append(dirty, ci)
						}
						st.buf[ci*width+x] |= bit
						pi += 4
					}
					rowOffset += width * 4
				}
			} else {
				for dy := 0; dy < nRows; dy++ {
					yb4 := ((job.yStart + dy) & 3) << 2
					bit := byte(1 << dy)
					r0, r1, r2, r3 := &pal.lutR[yb4|0], &pal.lutR[yb4|1], &pal.lutR[yb4|2], &pal.lutR[yb4|3]
					g0, g1, g2, g3 := &pal.lutG[yb4|0], &pal.lutG[yb4|1], &pal.lutG[yb4|2], &pal.lutG[yb4|3]
					b0, b1, b2, b3 := &pal.lutB[yb4|0], &pal.lutB[yb4|1], &pal.lutB[yb4|2], &pal.lutB[yb4|3]
					pi := rowOffset
					lim := width &^ 3
					for x := 0; x < lim; x += 4 {
						ci := int(r0[data[pi]]) | int(g0[data[pi+1]]) | int(b0[data[pi+2]])
						if st.seen[ci] != st.epoch {
							st.seen[ci] = st.epoch
							dirty = append(dirty, ci)
						}
						st.buf[ci*width+x] |= bit
						ci = int(r1[data[pi+4]]) | int(g1[data[pi+5]]) | int(b1[data[pi+6]])
						if st.seen[ci] != st.epoch {
							st.seen[ci] = st.epoch
							dirty = append(dirty, ci)
						}
						st.buf[ci*width+x+1] |= bit
						ci = int(r2[data[pi+8]]) | int(g2[data[pi+9]]) | int(b2[data[pi+10]])
						if st.seen[ci] != st.epoch {
							st.seen[ci] = st.epoch
							dirty = append(dirty, ci)
						}
						st.buf[ci*width+x+2] |= bit
						ci = int(r3[data[pi+12]]) | int(g3[data[pi+13]]) | int(b3[data[pi+14]])
						if st.seen[ci] != st.epoch {
							st.seen[ci] = st.epoch
							dirty = append(dirty, ci)
						}
						st.buf[ci*width+x+3] |= bit
						pi += 16
					}
					for x := lim; x < width; x++ {
						b := yb4 | (x & 3)
						ci := int(pal.lutR[b][data[pi]]) | int(pal.lutG[b][data[pi+1]]) | int(pal.lutB[b][data[pi+2]])
						if st.seen[ci] != st.epoch {
							st.seen[ci] = st.epoch
							dirty = append(dirty, ci)
						}
						st.buf[ci*width+x] |= bit
						pi += 4
					}
					rowOffset += width * 4
				}
			}

			st.dirty = dirty

				localBuf := sixelRLEBufPool.Get().(*bytes.Buffer)
				localBuf.Reset()
				encodeStrip(localBuf, st, job.sixelRow, width)
				rleBytes := make([]byte, localBuf.Len())
				copy(rleBytes, localBuf.Bytes())
				sixelRLEBufPool.Put(localBuf)

				if cache != nil {
					cache.mu.Lock()
					c := &cache.strips[job.sixelRow]
					c.rleData = append(c.rleData[:0], rleBytes...)
					c.pixelHash = pHash
					cache.mu.Unlock()
				}

				resultCh <- stripResult{sixelRow: job.sixelRow, rleData: rleBytes}
			}
		}()
	}

	jobsSent := 0
	for ; jobsSent < workers && jobsSent < totalSixelRows; jobsSent++ {
		yStart := jobsSent * 6
		yEnd := yStart + 6
		if yEnd > height {
			yEnd = height
		}
		jobCh <- stripJob{sixelRow: jobsSent, yStart: yStart, yEnd: yEnd}
	}

	makeJob := func(sixelRow int) stripJob {
		yStart := sixelRow * 6
		yEnd := yStart + 6
		if yEnd > height {
			yEnd = height
		}
		return stripJob{sixelRow: sixelRow, yStart: yStart, yEnd: yEnd}
	}

	nextRow := 0
	pending := make(map[int][]byte)
	recvCount := 0

	flushPending := func() {
		for {
			data, ok := pending[nextRow]
			if !ok {
				break
			}
			outBuf.Write(data)
			delete(pending, nextRow)
			nextRow++
		}
	}

	for recvCount < totalSixelRows {
		if jobsSent < totalSixelRows {
			select {
			case jobCh <- makeJob(jobsSent):
				jobsSent++
			case res := <-resultCh:
				recvCount++
				pending[res.sixelRow] = res.rleData
				if res.sixelRow == nextRow {
					flushPending()
				}
			}
		} else {
			res := <-resultCh
			recvCount++
			pending[res.sixelRow] = res.rleData
			if res.sixelRow == nextRow {
				flushPending()
			}
		}
	}

	close(jobCh)
	wg.Wait()

	outBuf.Write([]byte{0x1b, 0x5c})
	_, err := outBuf.WriteTo(w)
	return err
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
	return encodePaletted(w, paletted, width, height, nc, nil)
}

func encodePaletted(w io.Writer, paletted *image.Paletted, width, height, nc int, cache *SixelFrameCache) error {
	pix := paletted.Pix
	stride := paletted.Stride

	paletteSize := len(paletted.Palette)
	if paletteSize > nc {
		paletteSize = nc
	}

	estSize := width * height / 2
	if estSize < 65536 {
		estSize = 65536
	}
	outBuf := bytes.NewBuffer(make([]byte, 0, estSize))
	outBuf.Write([]byte{0x1b, 0x50, 0x30, 0x3b, 0x30, 0x3b, 0x38, 0x71, 0x22})
	writeSixelNum(outBuf, width)
	outBuf.WriteByte(';')
	writeSixelNum(outBuf, height)
	outBuf.Write([]byte{0x3b, 0x31, 0x3b, 0x31})
	for i := 0; i < paletteSize; i++ {
		r, g, b, _ := paletted.Palette[i].RGBA()
		outBuf.WriteByte('#')
		writeSixelNum(outBuf, i+1)
		outBuf.WriteString(";2;")
		writeSixelNum(outBuf, int(r*100/0xFFFF))
		outBuf.WriteByte(';')
		writeSixelNum(outBuf, int(g*100/0xFFFF))
		outBuf.WriteByte(';')
		writeSixelNum(outBuf, int(b*100/0xFFFF))
	}

	totalSixelRows := (height + 5) / 6
	workers := runtime.NumCPU()
	if workers > totalSixelRows {
		workers = totalSixelRows
	}
	if workers < 1 {
		workers = 1
	}

	jobCh := make(chan stripJob, workers)
	resultCh := make(chan stripResult, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				pHash := stripPixelHash(pix, stride, job.yStart, job.yEnd, width)

				if cache != nil {
					cache.mu.Lock()
					c := &cache.strips[job.sixelRow]
					if c.pixelHash == pHash && len(c.rleData) > 0 {
						rleCopy := make([]byte, len(c.rleData))
						copy(rleCopy, c.rleData)
						cache.mu.Unlock()
						resultCh <- stripResult{sixelRow: job.sixelRow, rleData: rleCopy}
						continue
					}
					cache.mu.Unlock()
				}

				st := sixelStripStatePool.Get().(*sixelStripState)
				st.pixelHash = pHash
				stripCap := nc * width
				if cap(st.buf) < stripCap {
					st.buf = make([]byte, stripCap)
				}
				st.buf = st.buf[:stripCap]
				if cap(st.seen) < nc {
					st.seen = make([]uint16, nc)
				} else {
					st.seen = st.seen[:nc]
				}
				st.epoch++
				if st.epoch == 0 {
					clear(st.seen)
					st.epoch = 1
				}
				clear(st.buf[:stripCap])
				dirty := st.dirty[:0]

				rowOffset := job.yStart * stride
				nRows := job.yEnd - job.yStart
				for dy := 0; dy < nRows; dy++ {
					bit := byte(1 << dy)
					for x := 0; x < width; x++ {
						idx := int(pix[rowOffset+x])
						if idx >= nc {
							continue
						}
						if st.seen[idx] != st.epoch {
							st.seen[idx] = st.epoch
							dirty = append(dirty, idx)
						}
						st.buf[idx*width+x] |= bit
					}
					rowOffset += stride
				}

				st.dirty = dirty
				resultCh <- stripResult{sixelRow: job.sixelRow, state: st}
			}
		}()
	}

	jobsSent := 0
	for ; jobsSent < workers && jobsSent < totalSixelRows; jobsSent++ {
		yStart := jobsSent * 6
		yEnd := yStart + 6
		if yEnd > height {
			yEnd = height
		}
		jobCh <- stripJob{sixelRow: jobsSent, yStart: yStart, yEnd: yEnd}
	}

	makeJob := func(sixelRow int) stripJob {
		yStart := sixelRow * 6
		yEnd := yStart + 6
		if yEnd > height {
			yEnd = height
		}
		return stripJob{sixelRow: sixelRow, yStart: yStart, yEnd: yEnd}
	}

	nextRow := 0
	pending := make(map[int]pendingItem)
	recvCount := 0

	for recvCount < totalSixelRows {
		if jobsSent < totalSixelRows {
			select {
			case jobCh <- makeJob(jobsSent):
				jobsSent++
			case res := <-resultCh:
				recvCount++
				processStripResult(outBuf, res, &nextRow, pending, width, cache)
			}
		} else {
			res := <-resultCh
			recvCount++
			processStripResult(outBuf, res, &nextRow, pending, width, cache)
		}
	}

	close(jobCh)
	wg.Wait()

	outBuf.Write([]byte{0x1b, 0x5c})
	_, err := outBuf.WriteTo(w)
	return err
}

func processStripResult(outBuf *bytes.Buffer, res stripResult, nextRow *int, pending map[int]pendingItem, width int, cache *SixelFrameCache) {
	writeOne := func(row int, item pendingItem) {
		if item.rleData != nil {
			outBuf.Write(item.rleData)
			return
		}
		st := item.state
		start := outBuf.Len()
		encodeStrip(outBuf, st, row, width)
		if cache != nil && st != nil {
			end := outBuf.Len()
			c := &cache.strips[row]
			c.rleData = append(c.rleData[:0], outBuf.Bytes()[start:end]...)
			c.pixelHash = st.pixelHash
		}
	}

	if res.sixelRow == *nextRow {
		writeOne(res.sixelRow, pendingItem{state: res.state, rleData: res.rleData})
		*nextRow++
		for {
			item, ok := pending[*nextRow]
			if !ok {
				break
			}
			writeOne(*nextRow, item)
			delete(pending, *nextRow)
			*nextRow++
		}
	} else {
		pending[res.sixelRow] = pendingItem{state: res.state, rleData: res.rleData}
	}
}

func encodeStrip(outBuf *bytes.Buffer, st *sixelStripState, sixelRow, width int) {
	if st == nil {
		return
	}

	if sixelRow > 0 {
		outBuf.WriteByte(0x2d)
	}

	for _, c := range st.dirty {
		base := c * width
		row := st.buf[base : base+width]

		outBuf.WriteByte(0x24)
		outBuf.WriteByte(0x23)
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
						outBuf.WriteByte(0x21)
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
