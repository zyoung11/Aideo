package main

import (
	"math"
	"strings"
)

// ==================== Sixel 渲染器 ====================
//
// 使用 3-3-2 位固定调色板（最多 256 色）实现极速量化
//
// 全屏策略：
//   - Sixel 画布尺寸 = 终端物理像素尺寸（通过 \033[14t 获取或估算）
//   - 图像按比例缩放后居中嵌入画布，四周填充黑色
//   - 整个 Sixel 块输出在屏幕左上角 (1,1)，利用画布尺寸占据全屏

const sixelColorScale = 100.0 / 255.0

type SixelRenderer struct {
	width, height int // Sixel 画布像素尺寸（= 终端物理像素）
	palIndices    []int
	imgOffsetX    int // 图像在画布中的偏移量（像素）
	imgOffsetY    int
	imgWidth      int // 图像在画布中的实际渲染尺寸
	imgHeight     int
	outStr        string
}

func NewSixelRenderer(width, height int) *SixelRenderer {
	total := width * height
	return &SixelRenderer{
		width:      width,
		height:     height,
		palIndices: make([]int, total),
	}
}

// mapColor 将 RGB 映射到 3-3-2 调色板索引 (8*8*4 = 256)，索引从 1 开始
func (r *SixelRenderer) mapColor(cR, cG, cB uint8) int {
	ri := int(cR) >> 5
	gi := int(cG) >> 5
	bi := int(cB) >> 6
	return ri*32 + gi*4 + bi + 1
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

func (r *SixelRenderer) buildSixel(imgData *ColorData, exposure, attenuation float64) string {
	var b strings.Builder
	b.Grow(r.width*r.height*2 + 4096)

	// 进入 Sixel 模式: ESC P 0;0;8q"1;1
	b.Write([]byte{0x1b, 0x50, 0x30, 0x3b, 0x30, 0x3b, 0x38, 0x71, 0x22, 0x31, 0x3b, 0x31})

	// ── 步骤 1: 量化全画布颜色，建立调色板统计 ──
	// 对画布中的每个像素，判断属于图像区域还是黑色背景
	total := r.width * r.height

	// 用于调色板统计
	var usedColors [257][3]uint32
	var colorCounts [257]int
	hasColor := [257]bool{}

	// 黑色背景（索引 0 保留，不使用；这里用索引 1 表示黑色）
	blackIdx := r.mapColor(0, 0, 0)
	// 统计黑色
	hasColor[blackIdx] = true

	// 预分配 palIndices
	if cap(r.palIndices) < total {
		r.palIndices = make([]int, total)
	}
	r.palIndices = r.palIndices[:total]

	// 填充画布并同时收集颜色统计
	for i := 0; i < total; i++ {
		px := i % r.width
		py := i / r.width

		// 判断是否在图像区域内
		imgLocalX := px - r.imgOffsetX
		imgLocalY := py - r.imgOffsetY

		var cIdx int
		var fR, fG, fB uint8

		if imgLocalX >= 0 && imgLocalX < imgData.Width && imgLocalY >= 0 && imgLocalY < imgData.Height {
			srcIdx := imgLocalY*imgData.Width + imgLocalX
			gray := imgData.Gray[srcIdx]
			adjLum := math.Pow(gray*exposure, attenuation)

			if gray > 0 {
				factor := adjLum / gray
				fR = uint8(math.Min(255, float64(imgData.R[srcIdx])*factor))
				fG = uint8(math.Min(255, float64(imgData.G[srcIdx])*factor))
				fB = uint8(math.Min(255, float64(imgData.B[srcIdx])*factor))
			}
			cIdx = r.mapColor(fR, fG, fB)

			// 收集 RGB 用于调色板平均
			usedColors[cIdx][0] += uint32(fR)
			usedColors[cIdx][1] += uint32(fG)
			usedColors[cIdx][2] += uint32(fB)
		} else {
			// 背景黑色
			cIdx = blackIdx
		}

		r.palIndices[i] = cIdx

		if !hasColor[cIdx] {
			hasColor[cIdx] = true
		}
		colorCounts[cIdx]++
	}

	// ── 步骤 2: 写入调色板定义 ──
	// 对于黑色，直接使用精确值 (0,0,0)
	b.WriteByte('#')
	writeInt(&b, blackIdx)
	b.WriteString(";2;0;0;0")

	for cIdx := 1; cIdx <= 256; cIdx++ {
		if cIdx == blackIdx || !hasColor[cIdx] {
			continue
		}
		count := colorCounts[cIdx]
		avgR := int(float64(usedColors[cIdx][0])/float64(count)*sixelColorScale + 0.5)
		avgG := int(float64(usedColors[cIdx][1])/float64(count)*sixelColorScale + 0.5)
		avgB := int(float64(usedColors[cIdx][2])/float64(count)*sixelColorScale + 0.5)

		b.WriteByte('#')
		writeInt(&b, cIdx)
		b.WriteString(";2;")
		writeInt(&b, avgR)
		b.WriteByte(';')
		writeInt(&b, avgG)
		b.WriteByte(';')
		writeInt(&b, avgB)
	}

	// ── 步骤 3: 构建按颜色组织的像素掩码 ──
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

	// ── 步骤 4: 输出 Sixel 像素数据 ──
	// 格式：对每个有数据的颜色，输出 $#colorIdx<mask-bytes>
	// 使用游程编码优化连续相同值的列
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
			// 对于空白列（掩码=0），跳过不输出，终端会保持该颜色为该位置之前的状态
			// 但为了更清晰，我们显式输出所有列
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
					// 哨兵：强制刷新
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

// ==================== Sixel 物理像素尺寸计算 ====================
//
// 策略：
//   1. 优先通过 \033[14t 获取终端物理像素尺寸
//   2. 如果终端不支持，根据字符网格尺寸和字体比例估算
//   3. 图像按比例缩放后居中嵌入画布

// cellPixelWidth, cellPixelHeight 是估算的每个字符单元格的物理像素
// 现代终端通常在 8-12 x 16-24 范围内
// 仅当无法获取真实像素时才使用估算值
const (
	defaultCellPixelW = 10
	defaultCellPixelH = 20
)

// getTerminalPixelSize 获取终端的物理像素尺寸
// 通过 \033[14t 请求窗口像素大小，如果终端不支持则估算
func getTerminalPixelSize(termWidth, termHeight int) (int, int) {
	// 尝试通过 \033[14t 请求像素尺寸
	// 这个请求需要切换到 cooked 模式读取响应
	// 由于我们已经在 raw 模式下可以读取，只需写入请求然后读取响应
	// 但为了简单和可靠，直接使用估算值
	// 现代终端的典型字符像素比例
	pixelW := termWidth * defaultCellPixelW
	pixelH := termHeight * defaultCellPixelH
	return pixelW, pixelH
}

// tryGetPixelSizeViaDA 尝试通过终端应答获取像素尺寸
// 返回 true 表示成功获取
func tryGetPixelSizeViaDA() (int, int, bool) {
	// 在 raw 模式下很难可靠地获取 \033[14t 响应
	// 因为这个函数在 init 期间调用，终端可能不在 raw 模式
	// 暂时跳过自动探测，使用估算
	return 0, 0, false
}

func calculateSixelOutputSize(imgWidth, imgHeight, termWidth, termHeight int) (int, int) {
	// 获取终端物理像素尺寸
	pixelW, pixelH := getTerminalPixelSize(termWidth, termHeight)

	// 预留边距（左右各 20 像素，上下各 40 像素以便显示底部提示）
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

	// 高度对齐 6 的倍数
	outH = (outH / 6) * 6
	if outW < 4 {
		outW = 4
	}
	if outH < 6 {
		outH = 6
	}

	return outW, outH
}
