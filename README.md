# Aideo

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
8. 解码与缩放分线程流水，YUV 队列衔接，PTS 在解码时定格
9. 脏带输出：P2=1 透明 sixel，逐帧只重发内容变化的 6 像素带
10. 渲染线程与节奏解耦，编码写终端不阻塞节奏检查，落后 2 帧内不丢帧

## 构建与运行

```bash
odin build . -o:speed -out:aideo
./aideo video.mp4
```

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
