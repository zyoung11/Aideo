# Aideo

> 终端图像与视频查看器 — 在终端中欣赏图片，播放视频

Aideo 是一个用 Go 编写的终端多媒体播放器，支持在终端中**渲染图片**和**播放视频**。它通过多种终端图像协议（Braille ASCII、Sixel、Kitty）实现从经典字符画到真彩色图像的完整覆盖，并能自动检测终端能力选择最佳渲染方式。

## ✨ 功能特性

### 图片查看

- **Braille ASCII 模式** — 使用 Unicode Braille 字符（⠿）实现 2×4 像素密度，呈现经典字符画效果
- **Sixel 真彩色模式** — 支持 255 色调色板，带 Floyd-Steinberg 抖动
- **Kitty 真彩色模式** — 使用 Kitty Graphics Protocol，支持 zlib 压缩传输
- **自动协议检测** — 自动识别终端支持的图像协议并选择最佳渲染方式
- **主色调分析** — 提取图像主导颜色
- **窗口自适应** — 终端 resize 时自动重新渲染
- **支持格式** — JPG、PNG

### 视频播放

- **FFmpeg 解码** — 支持 MP4、MOV、MKV 等主流格式
- **三模渲染** — Braille ASCII / Sixel / Kitty，自动选择
- **音频播放** — 内嵌音频解码与播放（使用 beep 库）
- **双缓冲循环** — 无缝循环播放，无黑帧
- **精确 Seek** — 窗口 resize 时通过 output-side `-ss` 实现精确跳转追帧
- **帧率自适应** — Kitty 模式下动态调整帧率，卡顿自动降帧
- **窗口自适应** — resize 时自动重新计算分辨率并 seek 到当前位置

### 技术亮点

- 高性能：缓冲区复用（sync.Pool）、并行 Sixel 编码、帧缓冲区复用
- 自动协议检测：检测 Kitty、WezTerm、Ghostty、rio、foot、mlterm、contour、mintty 等
- 优雅退出：panic 恢复、信号处理（SIGINT/SIGTERM/SIGHUP/SIGWINCH）、终端状态还原

## 📦 依赖

| 依赖                             | 用途              |
| ------------------------------ | --------------- |
| [ffmpeg](https://ffmpeg.org/)  | 视频/音频解码（需要系统安装） |
| `github.com/gopxl/beep/v2`     | 音频播放            |
| `github.com/nfnt/resize`       | 图像缩放（Lanczos3）  |
| `github.com/soniakeys/quant`   | 图像量化（中位切分）      |
| `github.com/u2takey/ffmpeg-go` | FFmpeg Go 封装    |
| `golang.org/x/term`            | 终端尺寸查询、raw 模式   |

> **注意**：系统需要安装 FFmpeg（`ffmpeg` 可执行文件），视频播放会调用项目根目录下的 `ffmpeg` / `ffmpeg.exe`。

## 🚀 快速开始

### 编译

```bash
go build .
```

### 查看图片

```bash
go run main.go photo.jpg
go run main.go photo.png
```

### 播放视频

```bash
go run main.go video.mp4
go run main.go clip.mov
go run main.go movie.mkv
```

### 退出

按 `q` 或 `ESC` 退出。

## 🏗️ 项目结构

```
Aideo/
├── main.go          # 主入口，图片查看逻辑
├── ascii.go         # Braille 渲染器、图像缩放、终端控制
├── video.go         # 视频播放器、FFmpeg 解码、音频流
├── image/
│   └── image.go     # Sixel/Kitty 编码器、协议检测、图像渲染
├── ffmpeg           # FFmpeg 二进制（Linux/Mac）
├── ffmpeg.exe       # FFmpeg 二进制（Windows）
├── go.mod
└── go.sum
```

## 📐 核心架构

### 图像渲染管线

```
图片文件 → 解码 (PNG/JPG) → RGBA 像素 → 双线性缩放 → Braille 渲染
                                                              ↓
                                                       前景/背景色分离
                                                       (中位数阈值分割)
                                                              ↓
                                                       ANSI 转义序列输出
```

### Braille 渲染原理

每个 Braille 字符覆盖 **2 列 × 4 行 = 8 个像素**：

```
  Dot1(0x01)  Dot4(0x08)    ← 第 0 行
  Dot2(0x02)  Dot5(0x10)    ← 第 1 行
  Dot3(0x04)  Dot6(0x20)    ← 第 2 行
  Dot7(0x40)  Dot8(0x80)    ← 第 3 行

字符 = U+2800 | 位或掩码
```

亮度使用 **ITU-R BT.709** 权重：`0.2126*R + 0.7152*G + 0.0722*B`，通过中位数将 8 个像素分为前景（亮）和背景（暗）两组，分别计算平均 RGB 颜色。

### 视频播放架构

```
┌─────────────────────────────────────────────────┐
│                   VideoPlayer                    │
│                                                  │
│  ┌──────────────┐    ┌──────────────────────┐   │
│  │ VideoDecoder │    │   AudioStreamer      │   │
│  │ (双缓冲)      │    │   (beep + FFmpeg)    │   │
│  │ currentDec   │    │                      │   │
│  │ nextDec      │    │  Stream() → beep     │   │
│  └──────┬───────┘    └──────────────────────┘   │
│         │                                        │
│         ▼                                        │
│  ┌──────────────────────────────────────────┐   │
│  │            帧渲染器                        │   │
│  │  ASCII / Sixel / Kitty (自动选择)         │   │
│  └──────────────────────────────────────────┘   │
└─────────────────────────────────────────────────┘
```

### 协议检测流程

```
检测 TMUX/ZELLIJ → 失败
    ↓
检测 KITTY_WINDOW_ID / TERM_PROGRAM (WezTerm/Ghostty/rio) → 成功 → Kitty
    ↓
检测 TERM (foot/mlterm/contour/xterm-sixel/...) → 成功 → Sixel
    ↓
查询 DEC 私有模式 → 成功 → Sixel
    ↓
失败 → 回退到 Braille ASCII
```
