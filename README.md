# Aideo

> 终端视频播放器 — Odin 实现，libav 解码，Sixel 渲染，SIMD 量化

## 项目结构

```
Aideo/
├── main.odin        # 入口，主循环，帧调度，按键与 resize
├── libav.odin       # libavformat/libavcodec/libswscale/libswresample C 绑定
├── video.odin       # 视频解码线程，帧池，无缝循环，resize 通道
├── audio.odin       # 音频解码，环形缓冲，miniaudio 输出，平滑时钟
├── sixel.odin       # Sixel 编码器，SIMD 量化，strip 缓存，常驻 worker 池
├── terminal.odin    # termios raw 模式，SIGWINCH，按键线程
├── ci_test.odin     # SIMD 与标量量化一致性回归
└── ci_bench.odin    # 量化与编码耗时基准
```

## 依赖

| 库 | 用途 |
| --- | --- |
| `libavformat` `libavcodec` | 容器解封装与解码（帧级多线程） |
| `libswscale` | YUV 到 RGBA 缩放 |
| `libswresample` | 音频重采样到 44100 Hz s16 |
| `vendor:miniaudio` | 音频输出设备 |

## 架构

1. libav 进程内解码，`threads=0` 启用帧级多线程，seek 后 `avcodec_flush_buffers` 重建解码链
2. 视频帧池自平衡调度，resize 后旧尺寸帧丢弃重建
3. 音频整轨解码进环形缓冲，声卡消费样本数经线性插值驱动主时钟，暂停冻结
4. 帧调度：墙钟平滑 pacing，音频时钟做 50 ms 容差慢漂移校正
5. 循环播放：EOF 后 seek 归零加 `loop_offset` 累积，音视频循环周期统一为视频时长，多圈不漂移
6. Sixel 编码：均匀量化档走 SSSE3 SIMD（通道分离、饱和加减抖动），Retro 档走 LUT
7. strip 编码由常驻线程池并行（工作窃取），strip RLE 缓存跳过静止区域

## 构建与运行

```bash
odin build . -o:speed
./Aideo video.mp4
```

## 按键

| 键 | 功能 |
| --- | --- |
| `q` / `ESC` | 退出 |
| `空格` | 暂停 / 恢复 |
| `j` / `k` | 色数降档 / 升档（2/8/16/32/64/128/256） |
| `↑` / `↓` | 音量增 / 减 |

终端 resize 自适应，不中断播放。

## 测试

```bash
./Aideo citest    # SIMD 与标量量化一致性
./Aideo cibench   # 量化与编码耗时基准
```
