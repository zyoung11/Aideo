package main

import (
	"fmt"
	"image"
	"os"
	"os/signal"
	"strings"
	"syscall"

	timage "Aideo/image"

	"github.com/nfnt/resize"
	"golang.org/x/term"
)

func runImagePlayer(filePath string) error {
	fmt.Print(ENTER_ALTERNATE)
	fmt.Print(HIDE_CURSOR)
	fmt.Print(DISABLE_MOUSE)
	defer func() {
		fmt.Print(ENABLE_MOUSE)
		fmt.Print(SHOW_CURSOR)
		fmt.Print(LEAVE_ALTERNATE)
	}()

	renderFullImage(filePath)

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %v", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

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
				if b == 27 && i+1 < n {
					next := buf[i+1]
					if next == '[' || next == 'O' || next == 'M' {
						i++
						for i < n && !isTerminator(buf[i]) {
							i++
						}
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

	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGWINCH:
				fmt.Print(CLEAR_SCREEN + "\x1b[1;1H")
				renderFullImage(filePath)
			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP:
				return nil
			}
		case key, ok := <-keyCh:
			if !ok {
				return nil
			}
			switch key {
			case 'q', 'Q', 27:
				return nil
			}
		}
	}
}

func renderFullImage(filePath string) {
	protocol, hasProtocol := timage.DetectCapableProtocol()
	if hasProtocol {
		renderImageProtocol(filePath, protocol)
	} else {
		renderImageBrailleFull(filePath)
	}
}

func renderImageProtocol(filePath string, protocol timage.Protocol) {
	cellW, cellH := timage.CellPixels()
	if cellW < 1 {
		cellW = 8
	}
	if cellH < 1 {
		cellH = 16
	}

	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		w, h = 80, 24
	}

	img, err := loadImageAsRGBA(filePath)
	if err != nil {
		return
	}

	targetW := w * cellW
	targetH := h * cellH

	scaled := resize.Thumbnail(uint(targetW), uint(targetH), img, resize.Lanczos3)
	finalW, finalH := scaled.Bounds().Dx(), scaled.Bounds().Dy()
	imageCols := (finalW + cellW - 1) / cellW
	imageRows := (finalH + cellH - 1) / cellH
	if imageCols > w {
		imageCols = w
	}
	if imageRows > h {
		imageRows = h
	}
	if imageCols < 1 {
		imageCols = 1
	}
	if imageRows < 1 {
		imageRows = 1
	}

	startCol := (w - imageCols) / 2
	startRow := (h - imageRows) / 2
	if startCol < 0 {
		startCol = 0
	}
	if startRow < 0 {
		startRow = 0
	}
	startCol++
	startRow++

	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)
	fmt.Printf("\x1b[%d;%dH", startRow, startCol)

	switch protocol {
	case timage.ProtocolKitty:
		timage.EncodeKittyFrame(os.Stdout, scaled, imageCols, imageRows)
	default:
		scaledRGBA := image.NewRGBA(scaled.Bounds())
		for y := 0; y < finalH; y++ {
			for x := 0; x < finalW; x++ {
				scaledRGBA.Set(x, y, scaled.At(x, y))
			}
		}
		timage.EncodeSixelFrameRaw(os.Stdout, scaledRGBA.Pix, finalW, finalH, DefaultVideoColors, false, nil)
	}

	fillCol := startCol + imageCols
	if fillCol <= w {
		for row := startRow; row < startRow+imageRows; row++ {
			fmt.Printf("\x1b[%d;%dH\x1b[K", row, fillCol)
		}
	}
}

func renderImageBrailleFull(filePath string) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		w, h = 80, 24
	}

	imgData, err := loadImage(filePath)
	if err != nil {
		return
	}

	outW, outH := calculateOutputSize(imgData.Width, imgData.Height, w, h)
	scaledData := resizeImageBilinear(imgData, outW, outH)

	charW := outW / 2
	charH := outH / 4

	br := NewBrailleRenderer(charW, charH)
	br.Render(scaledData, 1.0, 0.85)

	startCol := (w - charW) / 2
	startRow := (h - charH) / 2
	if startCol < 0 {
		startCol = 0
	}
	if startRow < 0 {
		startRow = 0
	}

	fmt.Print(CLEAR_SCREEN + CURSOR_HOME)

	imageStr := br.String()
	lines := strings.Split(imageStr, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		fmt.Printf("\x1b[%d;%dH%s", startRow+i+1, startCol+1, line)
	}

	if charW > 0 && startCol+charW <= w {
		clearStartCol := startCol + charW + 1
		for row := startRow + 1; row <= startRow+charH; row++ {
			fmt.Printf("\x1b[%d;%dH\x1b[K", row, clearStartCol)
		}
	}
}
