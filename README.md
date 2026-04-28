# Aideo

> 终端多媒体播放器 — Go 实现，Sixel 为核心渲染协议

## 项目结构

```
Aideo/
├── main.go           # 入口，Braille 图片查看
├── ascii.go          # Braille 渲染器，双线性缩放
├── video.go          # 视频播放器，FFmpeg 解码，音频同步
├── image/
│   ├── image.go      # 公共类型，ShowImage，协议检测
│   ├── sixel.go      # Sixel 编码管线（~850 行）
│   └── kitty.go      # Kitty 协议编码
└── ffmpeg            # 静态编译备用
```

## 支持的终端协议

协议检测优先级：Kitty → Sixel → Braille ASCII

## FFmpeg 视频解码

1. 探测视频元数据（`ffprobe` 解析 JSON）：宽度、高度、帧率、总帧数、时长
2. 检测音频流是否存在，有则用 beep 库异步播放 PCM s16le 流
3. 启动双缓冲解码器：同时维护 `currentDec` 和 `nextDec` 两个 ffmpeg 进程
4. 当前解码器结束时无缝切换到下一个，`nextDec` 立即补充新进程
5. 解码命令：`ffmpeg -i input -f rawvideo -pix_fmt rgba -s WxH pipe:`
6. 帧通过独立 goroutine 异步读取，通过 buffered channel 传给主循环
7. resize 时：关闭旧解码器，用 `output -ss` 精确 seek 到当前时间前 0.05 秒，启动新解码器，通知 reader goroutine 切换，drain 旧尺寸帧，`frameCount` 对齐 seek 位置
8. A/V 同步：以 `playbackStart + frameCount × frameTime` 为预期时间轴，落后半帧则从 channel drain 跳帧，超前则 sleep

## Sixel 编码管线

### 1. 帧缓存检查

6 行为一个 strip。每帧对每个 strip 计算其原始 RGBA 数据的 FNV-1a 64 位哈希，与上一帧同一 strip 的哈希比对。命中则直接复用缓存的 RLE 字节，跳过后续全部步骤。未命中则进入量化。

### 2. 调色板选择

视频模式 7 档手动切换（`k` 升 `j` 降），图片模式固定 256 色：

| 档   | 色数  | R bit | G bit | B bit | 调色板                                                 |
| --- | --- | ----- | ----- | ----- | --------------------------------------------------- |
| 2   | 2   | —     | —     | —     | 灰度黑白，BT.709 亮度加权 + Bayer 抖动                         |
| 8   | 8   | 1     | 1     | 1     | 均匀量化                                                |
| 16  | 16  | 2     | 1     | 1     | Retro 暖琥珀滤镜：R {0,105,210,255}，G {20,200}，B {10,150} |
| 32  | 32  | 2     | 2     | 1     | Retro 暖琥珀滤镜：R/G {0,105,210,255}，B {10,150}          |
| 64  | 64  | 2     | 2     | 2     | 均匀量化（默认）                                            |
| 128 | 128 | 3     | 2     | 2     | 均匀量化                                                |
| 256 | 256 | 3     | 3     | 2     | 均匀量化（图片模式）                                          |

换档时帧缓存自动重建（不同色板产生不同 RLE 输出）。

### 3. 调色板初始化

每种色板在首次使用时预计算两个数据结构：颜色表和量化 LUT。

颜色表 `palette.colors[]`：根据各通道的 bit 数和级数，生成所有组合的 RGB 值。Retro 档使用自定义级数替代均匀分布。

量化 LUT `palette.lutR/G/B[16][256]`：3 个 16×256 字节的查找表，共 12KB，L1 缓存常驻。每个 Bayer 位置（16 种）预计算 256 个原始字节值的输出值：

```
输入：rawByte ∈ [0, 255]
步骤：rawByte + dither[bayerPos] → clamp(0, 255) → >> (8 - bits) → << shift
结果：预移位的调色板索引贡献值（R 的贡献在 bits 5-4，G 在 3-2，B 在 1-0）
```

最终索引由三个 LUT 输出直接 OR 得到，无需移位：`ci = lutR[d[pi]] | lutG[d[pi+1]] | lutB[d[pi+2]]`。

### 4. Bayer 有序抖动

使用 4×4 Bayer 矩阵，抖动幅度 = `256/(级数-1)/2`。每通道独立计算自己的抖动表 `makeDither(levels)`，在 LUT 预计算时融入表中。像素处理时无额外抖动计算开销。

### 5. 并行 strip 量化

帧按 6 行分组为 strip。创建 `runtime.NumCPU()` 个 worker goroutine，每个 worker 从 `sync.Pool` 取 `sixelStripState`（包含 `nc×width` 字节位图 + `nc×2` 字节 epoch 数组）。

每个 strip 处理流程：

a. 哈希检查 → 缓存命中则直接返回 `[]byte` RLE 数据
b. 一次性 `clear(位图)` 清零（`nc×width` 字节，64 色时 128KB）
c. 遍历该 strip 的 6 行 × width 像素，每行主循环 4 路展开：

```
预计算本行 4 个 Bayer 位置的 LUT 指针（3×4=12 个指针）
pi = rowOffset，步进 pi += 16 处理 4 像素/迭代
每个像素：3 次 L1 查表 + 2 次 OR → 调色板索引
epoch 标记首次出现的颜色到 dirty 列表
位图写入：buf[ci*width + x] |= (1 << dy)
```

d. RLE 编码：遍历 dirty 颜色列表，扫描对应颜色的位图行，生成 `$#N!count?` 格式的 Sixel sixel 数据，输出到池化 `bytes.Buffer`
e. 更新帧缓存：缓存 RLE 字节 + 像素哈希
f. 通过 channel 返回 `{sixelRow, rleBytes}` 给主线程

### 6. 主线程拼装

1. 输出 Sixel DCS 头部：`ESC P 0;0;8q "W;H;1;1`
2. 输出调色板：对每个条目 `#N;2;R;G;B`
3. 从 channel 接收各 strip 的 RLE 字节，按 strip 序号保序写入输出缓冲（用 `map[int][]byte` 暂存乱序结果）
4. 所有 strip 完成后输出 Sixel 终止符 `ESC \`
5. `outBuf.WriteTo(w)` 写入终端

### 7. 终端输出

编码完成后，附加光标定位、清除残留区域、状态栏，通过 `os.Stdout.Write` 一次性写入。状态栏显示：分辨率、色数、实时帧率、编码耗时。

## Grayscale 模式

2 色灰度使用独立路径：每像素计算 ITU-R BT.709 加权亮度 `(6966R + 23436G + 2366B) >> 15`，加 Bayer 抖动后 `>> 7` 得 0/1。位图仅 2×width 字节。



## 使用

```bash
go build -ldflags="-s -w" .
./Aideo photo.jpg     # 图片
./Aideo video.mp4     # 视频（带音频）
```

图片：按 `q`/`ESC` 退出。终端 resize 自适应。

视频：按 `q`/`ESC` 退出，`k`/`j` 切换色数。终端 resize 时不中断播放。

## Go 依赖

| 库                              | 用途                              |
| ------------------------------ | ------------------------------- |
| `github.com/gopxl/beep/v2`     | 音频播放                            |
| `github.com/nfnt/resize`       | Lanczos3 图像缩放                   |
| `github.com/soniakeys/quant`   | median cut 量化（图片 median cut 路径） |
| `github.com/u2takey/ffmpeg-go` | FFmpeg 进程管理                     |
| `golang.org/x/term`            | 终端尺寸、raw 模式                     |
