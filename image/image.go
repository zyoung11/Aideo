package image

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nfnt/resize"
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
	parts := bytes.SplitN(buf[2:len(buf)-1], []byte(";"), 3)
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
	if term := strings.ToLower(os.Getenv("TERM_PROGRAM")); term == "ghostty" {
		return false
	}
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
