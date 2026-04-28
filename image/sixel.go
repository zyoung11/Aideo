package image

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/soniakeys/quant/median"
)

func renderSixel(img image.Image, colors int, dither bool) error {
	return EncodeSixelFrame(os.Stdout, img, colors, dither)
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

func retroLevels(levels int) []uint8 {
	lv := make([]uint8, levels)
	switch levels {
	case 2:
		lv[0], lv[1] = 20, 200
	case 4:
		lv[0], lv[1], lv[2], lv[3] = 0, 105, 210, 255
	default:
		for i := 0; i < levels; i++ {
			lv[i] = uint8(i * 255 / (levels - 1))
		}
	}
	return lv
}

func retroBlueLevels(levels int) []uint8 {
	lv := make([]uint8, levels)
	switch levels {
	case 2:
		lv[0], lv[1] = 10, 150
	default:
		for i := 0; i < levels; i++ {
			lv[i] = uint8(i * 255 / (levels - 1))
		}
	}
	return lv
}

func newRetroPalette(rBits, gBits, bBits int) *sixelPalette {
	rLevels := 1 << rBits
	gLevels := 1 << gBits
	bLevels := 1 << bBits
	nc := rLevels * gLevels * bLevels
	gShift := bBits
	rShift := gBits + bBits

	p := &sixelPalette{nc: nc, colors: make([]color.Color, nc)}
	rLv := retroLevels(rLevels)
	gLv := retroLevels(gLevels)
	bLv := retroBlueLevels(bLevels)
	for ri := 0; ri < rLevels; ri++ {
		for gi := 0; gi < gLevels; gi++ {
			for bi := 0; bi < bLevels; bi++ {
				p.colors[ri<<rShift|gi<<gShift|bi] = color.RGBA{
					rLv[ri], gLv[gi], bLv[bi], 255,
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
			var p *sixelPalette
			if nc == 16 || nc == 32 {
				p = newRetroPalette(bits[0], bits[1], bits[2])
			} else {
				p = newSixelPalette(bits[0], bits[1], bits[2])
			}
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

var sixelRLEBufPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 4096))
	},
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
