# Aideo

## 依赖

| 库 | 用途 |
| --- | --- |
| `libavformat` `libavcodec` `libavutil` | 解封装与解码（帧级多线程） |
| `libswscale` | 非 YUV420P 帧的格式转换兑底 |
| `libswresample` | 音频重采样到 44100 Hz s16 |
| `dav1d` | AV1 解码 |
| `zlib` | PNG 与 TIFF 解压 |
| `miniaudio` | 音频输出设备 |
| `avx2_quant.o` | 量化与缩放的 AVX2 与 AVX-512 内核，`cc` 编译 |

以上除 `avx2_quant.o` 由构建脚本编译外，全部静态编入二进制，运行时除 glibc 外无依赖。

## 支持格式

| 类别 | 格式 |
| --- | --- |
| 容器 | MP4、MOV、MKV、WebM、AVI、MPEG-TS、OGG、WAV、FLAC、MP3、FLV |
| 视频编码 | H.264、HEVC、VP8、VP9、AV1 |
| 音频编码 | AAC、MP3、FLAC、Opus、Vorbis、AC3、E-AC3、PCM s16le、s24le、f32le |
| 静态图 | PNG、JPEG、WebP、BMP、GIF、TIFF，含 EXIF 方向 |
| 动图 | GIF、APNG、动画 WebP，无限循环播放 |

## 架构

1. libav 进程内解码，帧级多线程，seek 后 `avcodec_flush_buffers` 重建解码链
2. 视频帧池自平衡调度，resize 后旧尺寸帧丢弃重建
3. 音频整轨解码进环形缓冲，声卡消费样本数经线性插值驱动主时钟，暂停冻结
4. 帧调度：墙钟平滑 pacing，音频时钟做 50 ms 容差慢漂移校正
5. 循环播放：EOF 排空解码器后归零重播，容器不支持 seek 时重开文件，动图按实际帧长无限循环
6. Sixel 编码：均匀量化与哈希走 AVX2 与 AVX-512 内核，Retro 档走 LUT
7. strip 编码由常驻线程池并行（原子抢任务），strip RLE 缓存跳过静止区域
8. 解码与缩放分线程流水，YUV 队列衔接，PTS 在解码时定格
9. 脏带输出：P2=1 透明 sixel，逐帧只重发内容变化的 6 像素带
10. 渲染线程与节奏解耦，编码写终端不阻塞节奏检查，落后 2 帧内不丢帧
11. 静图显示解码后按 EXIF 方向旋转，动图走视频管线无限循环
12. 画面恢复：焦点进入与终端 resize 清屏全量重绘，另每 2 秒全量一帧兜底

## 构建与运行

```bash
cc -O3 -mavx2 -c avx2_quant.c -o avx2_quant.o
odin build . -o:speed -out:aideo
./aideo video.mp4
```

## 选项

| 选项 | 功能 |
| --- | --- |
| `-c, --colors <n>` | 色数，2 8 16 32 64 128 256 |
| `-v, --volume <n>` | 音量，0 至 100 |
| `-t, --start <time>` | 起始位置，秒或 mm:ss 或 hh:mm:ss |
| `-s, --size <mode>` | 画面尺寸，full 或 half |
| `-p, --profile` | 退出时写性能报告到 `aideo_prof.txt` |
| `-h, --help` | 帮助页 |
| `--version` | 版本 |

## tmux

需要 tmux 3.4 及以上且编译启用 `--enable-sixel`。焦点重绘依赖焦点事件转发，在 `~/.tmux.conf` 启用并重载：

```
set -g focus-events on
```

切换 pane、tab、session 回到播放画面时自动重绘。不在 tmux 中运行时无此要求。

## 按键

| 键 | 功能 |
| --- | --- |
| `q` / `ESC` | 退出 |
| `空格` | 暂停 / 恢复 |
| `j` / `k` | 色数降档 / 升档（2/8/16/32/64/128/256） |
| `↑` / `↓` | 音量增 / 减 |
| `Tab` | 显示 / 隐藏帧率标签 |
| `m` | 静音 / 取消静音 |
| `f` | 全屏 / 半屏画面切换 |
| `←` / `→` | 后退 / 前进 5 秒 |

终端 resize 自适应，不中断播放。

## 测试

```bash
./aideo citest    # 量化与哈希的 SIMD 一致性
./aideo cibench   # 量化与编码耗时基准
./aideo cisws     # 缩放器逐字节一致性与耗时
./aideo ciframe   # 单帧像素对照诊断
```
