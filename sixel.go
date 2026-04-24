package main

import (
	"image"
	"image/color"
	"math"
	"strings"

	"github.com/soniakeys/quant"
	"github.com/soniakeys/quant/median"
)

// ==================== Sixel 渲染器 ====================
//
// 使用 中值切割量化 (Median Cut) 实现高精度 255 色调色板 + 黑色背景 = 256 色
//
// 全屏策略：
//   - Sixel 画布尺寸 = 终端物理像素尺寸（通过 \033[14t 获取或估算）
//   - 图像按比例缩放后居中嵌入画布，四周填充黑色
//   - 整个 Sixel 块输出在屏幕左上角 (1,1)，利用画布尺寸占据全屏

type SixelRenderer struct {
	width, height int             // Sixel 画布像素尺寸（= 终端物理像素）
	palette       []color.Color // 调色板 [1]=黑色背景, [2..256]=量化颜色
	palIndices    []int         // 每个画布像素对应的调色板索引
	imgOffsetX    int           // 图像在画布中的偏移量（像素）
	imgOffsetY    int
	imgWidth      int           // 图像在画布中的实际渲染尺寸
	imgHeight     int
	outStr        string

	// 量化器结果
	quantPal quant.Palette // 中值切割量化调色板
}

func NewSixelRenderer(width, height int) *SixelRenderer {
	total := width * height
	return &SixelRenderer{
		width:      width,
		height:     height,
		palette:    make([]color.Color, 257), // 1-indexed, 最大 256 色
		palIndices: make([]int, total),
	}
}

// setImagePlacement 计算图像在画布中的居中位置
func (r *SixelRenderer) setImagePlacement(imgW, imgH int) {
	r.imgWidth = imgW
	r.imgHeight = imgH
	r.imgOffsetX = (r.width - imgW) / 2
	r.imgOffsetY = (r.height - imgH) / 2
	if r.imgOffsetX < 0 {
		r.imgOffsetX = 0
	}
	if r.imgOffsetY < 0 {
		r.imgOffsetY = 0
	}
}

func (r *SixelRenderer) Render(imgData *ColorData, exposure, attenuation float64) {
	r.setImagePlacement(imgData.Width, imgData.Height)
	r.outStr = r.buildSixel(imgData, exposure, attenuation)
}

func (r *SixelRenderer) String() string {
	return r.outStr
}

// ==================== 中值切割量化 ====================

// colorDataToRGBA 将 ColorData 转换为 *image.RGBA，供量化器使用
func colorDataToRGBA(src *ColorData) *image.RGBA {
	bounds := image.Rect(0, 0, src.Width, src.Height)
	dst := image.NewRGBA(bounds)
	for y := 0; y < src.Height; y++ {
		for x := 0; x < src.Width; x++ {
			idx := y*src.Width + x
			i := y*dst.Stride + x*4
			dst.Pix[i+0] = src.R[idx]
			dst.Pix[i+1] = src.G[idx]
			dst.Pix[i+2] = src.B[idx]
			dst.Pix[i+3] = 255
		}
	}
	return dst
}

// medianCutQuantize 对图像进行 255 色中值切割量化
// 返回: quantPal (调色板查询器), stdPal (标准 color.Palette)
func medianCutQuantize(img *ColorData) (quant.Palette, color.Palette) {
	srcRGBA := colorDataToRGBA(img)

	// 中值切割量化到 255 色
	// median.Quantizer 实现中值切割算法，返回 TreePalette (二叉树，O(log n) 查找)
	qp := median.Quantizer(255).Palette(srcRGBA)

	// 获取标准颜色调色板
	stdPal := qp.ColorPalette()

	return qp, stdPal
}

// ==================== Sixel 构建 ====================

func (r *SixelRenderer) buildSixel(imgData *ColorData, exposure, attenuation float64) string {
	var b strings.Builder
	b.Grow(r.width*r.height*2 + 4096)

	// 进入 Sixel 模式: ESC P 0;0;8q"1;1
	b.Write([]byte{0x1b, 0x50, 0x30, 0x3b, 0x30, 0x3b, 0x38, 0x71, 0x22, 0x31, 0x3b, 0x31})

	// ── 步骤 1: 中值切割量化图像 ──
	qp, stdPal := medianCutQuantize(imgData)
	r.quantPal = qp

	// 构建调色板: [1] = 黑色背景, [2..256] = 量化颜色
	palette := make([]color.Color, 257)
	palette[1] = color.NRGBA{0, 0, 0, 255} // 黑色背景

	// 复制量化结果的颜色
	for i, c := range stdPal {
		if i+2 < 257 {
			palette[i+2] = c
		}
	}
	r.palette = palette

	// ── 步骤 2: 为画布每个像素分配调色板索引 ──
	total := r.width * r.height
	if cap(r.palIndices) < total {
		r.palIndices = make([]int, total)
	}
	r.palIndices = r.palIndices[:total]

	blackIdx := 1 // 黑色背景固定为索引 1

	for i := 0; i < total; i++ {
		px := i % r.width
		py := i / r.width

		imgLocalX := px - r.imgOffsetX
		imgLocalY := py - r.imgOffsetY

		if imgLocalX >= 0 && imgLocalX < imgData.Width && imgLocalY >= 0 && imgLocalY < imgData.Height {
			// 图像区域：应用曝光/衰减调整，然后查找调色板索引
			srcIdx := imgLocalY*imgData.Width + imgLocalX
			gray := imgData.Gray[srcIdx]
			adjLum := math.Pow(gray*exposure, attenuation)

			// 调整 RGB 亮度（与原有逻辑一致）
			var adjR, adjG, adjB uint8
			if gray > 0 {
				factor := adjLum / gray
				adjR = uint8(math.Min(255, float64(imgData.R[srcIdx])*factor))
				adjG = uint8(math.Min(255, float64(imgData.G[srcIdx])*factor))
				adjB = uint8(math.Min(255, float64(imgData.B[srcIdx])*factor))
			}

			// 使用量化调色板查找最接近的颜色索引 (0-based)
			adjColor := color.NRGBA{adjR, adjG, adjB, 255}
			qIdx := qp.IndexNear(adjColor) // 0-based index into quantized palette

			// 转换为 1-based: 量化颜色从索引 2 开始
			cIdx := qIdx + 2
			if cIdx > 256 {
				cIdx = 256
			}
			r.palIndices[i] = cIdx
		} else {
			// 背景黑色
			r.palIndices[i] = blackIdx
		}
	}

	// ── 步骤 3: 写入调色板定义 ──
	// Sixel 颜色寄存器格式: #<id>;2;<R>;<G>;<B>
	// R,G,B 范围 0-100 (百分比)

	// 黑色背景 (#1)
	b.WriteString("#1;2;0;0;0")

	// 量化颜色 (#2..#256)
	for ci := 2; ci <= 256; ci++ {
		c := r.palette[ci]
		if c == nil {
			continue
		}

		// 转换为 NRGBA 获取 8-bit RGB
		var nc color.NRGBA
		switch cc := c.(type) {
		case color.NRGBA:
			nc = cc
		case color.RGBA:
			nc = color.NRGBA{R: cc.R, G: cc.G, B: cc.B, A: 255}
		default:
			nc = color.NRGBAModel.Convert(c).(color.NRGBA)
		}

		// 将 8-bit (0-255) 映射到 sixel 百分比 (0-100)
		// 与文档中 R% = r * 100 / 0xFFFF 等价
		avgR := int(float64(nc.R)*100.0/255.0 + 0.5)
		avgG := int(float64(nc.G)*100.0/255.0 + 0.5)
		avgB := int(float64(nc.B)*100.0/255.0 + 0.5)

		// 确保在有效范围内
		if avgR > 100 {
			avgR = 100
		}
		if avgG > 100 {
			avgG = 100
		}
		if avgB > 100 {
			avgB = 100
		}

		b.WriteString("#")
		writeInt(&b, ci)
		b.WriteString(";2;")
		writeInt(&b, avgR)
		b.WriteByte(';')
		writeInt(&b, avgG)
		b.WriteByte(';')
		writeInt(&b, avgB)
	}

	// ── 步骤 4: 构建按颜色组织的像素掩码 ──
	sixelRows := (r.height + 5) / 6
	nc := 256

	type sixelStrip struct {
		masks [][]uint8 // [colorIdx][x] = bitmask
	}

	strips := make([]*sixelStrip, sixelRows)
	for z := 0; z < sixelRows; z++ {
		s := &sixelStrip{
			masks: make([][]uint8, nc+1),
		}
		for ci := 1; ci <= nc; ci++ {
			s.masks[ci] = make([]uint8, r.width)
		}
		strips[z] = s
	}

	for py := 0; py < r.height; py++ {
		z := py / 6
		if z >= sixelRows {
			z = sixelRows - 1
		}
		bit := uint8(py % 6)
		strip := strips[z]
		for px := 0; px < r.width; px++ {
			ci := r.palIndices[py*r.width+px]
			if ci < 1 || ci > nc {
				continue
			}
			strip.masks[ci][px] |= 1 << bit
		}
	}

	// ── 步骤 5: 输出 Sixel 像素数据 ──
	// 格式: $#colorIdx<mask-bytes>
	// 使用游程编码 (RLE) 优化连续相同值的列
	for z := 0; z < sixelRows; z++ {
		strip := strips[z]

		for ci := 1; ci <= nc; ci++ {
			masks := strip.masks[ci]

			// 跳过无数据的颜色
			hasData := false
			for x := 0; x < r.width; x++ {
				if masks[x] != 0 {
					hasData = true
					break
				}
			}
			if !hasData {
				continue
			}

			// $ 回车到第 0 列 + # 选择颜色
			b.WriteByte('$')
			b.WriteByte('#')
			writeInt(&b, ci)

			// 逐列输出，使用游程编码
			// 空白列（掩码=0）跳过不输出，终端保持该位置之前的颜色状态
			var lastCh uint8 = 0
			runLen := 0

			flushRun := func() {
				if runLen <= 0 {
					return
				}
				if runLen > 1 {
					b.WriteByte('!')
					writeInt(&b, runLen)
				}
				b.WriteByte(byte(63 + lastCh))
				runLen = 0
			}

			for x := 0; x <= r.width; x++ {
				var ch uint8
				if x < r.width {
					ch = masks[x]
				} else {
					// 哨兵：强制刷新最后一个 run
					ch = 0xFF
				}

				if ch != lastCh || runLen == 255 {
					flushRun()
					lastCh = ch
					runLen = 1
				} else {
					runLen++
				}
			}
		}

		// 条带分隔符
		if z < sixelRows-1 {
			b.WriteByte('-')
		}
	}

	b.WriteByte(0x1b) // ESC \
	b.WriteByte(0x5c)
	b.WriteString(RESET_COLORS)

	return b.String()
}

// ==================== 辅助函数 ====================

// writeInt 快速将整数写入 strings.Builder
func writeInt(b *strings.Builder, n int) {
	if n == 0 {
		b.WriteByte('0')
		return
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	b.Write(buf[i:])
}

// ==================== Sixel 物理像素尺寸计算 ====================

const (
	defaultCellPixelW = 10
	defaultCellPixelH = 20
)

func getTerminalPixelSize(termWidth, termHeight int) (int, int) {
	pixelW := termWidth * defaultCellPixelW
	pixelH := termHeight * defaultCellPixelH
	return pixelW, pixelH
}

func tryGetPixelSizeViaDA() (int, int, bool) {
	return 0, 0, false
}

func calculateSixelOutputSize(imgWidth, imgHeight, termWidth, termHeight int) (int, int) {
	pixelW, pixelH := getTerminalPixelSize(termWidth, termHeight)

	marginX := 20
	marginY := 40
	if pixelW <= marginX*2 {
		marginX = 0
	}
	if pixelH <= marginY {
		marginY = 0
	}

	availW := pixelW - marginX*2
	availH := pixelH - marginY

	if availW < 100 {
		availW = 100
	}
	if availH < 100 {
		availH = 100
	}

	imgAspect := float64(imgWidth) / float64(imgHeight)
	cellAspect := float64(availW) / float64(availH)

	var outW, outH int
	if imgAspect > cellAspect {
		outW = availW
		outH = int(math.Round(float64(outW) / imgAspect))
	} else {
		outH = availH
		outW = int(math.Round(float64(outH) * imgAspect))
	}

	outH = (outH / 6) * 6
	if outW < 4 {
		outW = 4
	}
	if outH < 6 {
		outH = 6
	}

	return outW, outH
}
