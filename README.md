# Aideo

A video player for the terminal and a sixel rendering engine in one, written in Odin. FFmpeg decodes in process and every frame is drawn as sixel. The engine also builds as a static library with a C ABI, see [libaideo](#libaideo).

## How it works

- Decode and scale run on separate thread pools connected by a YUV queue, PTS is fixed at decode time
- The audio ring buffer drives the master clock from consumed samples, the wall clock paces video with slow drift correction
- Quantization and hashing run on AVX2 and AVX-512 kernels, retro palettes use a lookup table
- Bands encode in parallel on a resident strip pool, a strip RLE cache skips unchanged regions
- Only changed 6-pixel bands are re-emitted as P2=1 transparent sixel
- Focus regain and terminal resize trigger a full repaint, with a full frame every 2 seconds as a safety net
- Animated images loop forever by frame duration, stills rotate by EXIF orientation

## Features

| Category | Formats |
|------|------|
| Containers | MP4, MOV, MKV, WebM, AVI, MPEG-TS, OGG, WAV, FLAC, MP3, FLV |
| Video | H.264, HEVC, VP8, VP9, AV1 |
| Audio | AAC, MP3, FLAC, Opus, Vorbis, AC3, E-AC3, PCM s16le, s24le, f32le |
| Still images | PNG, JPEG, WebP, BMP, GIF, TIFF |
| Animated | GIF, APNG, animated WebP |

## Build

Dependencies: `cc`, `nasm`, `meson`, `ninja`, `pkg-config`, `odin`.

```bash
cc -O3 -mavx2 -c avx2_quant.c -o avx2_quant.o
sh build_ffstatic.sh                 # static FFmpeg, dav1d, zlib
odin build cmd/aideo -o:speed -out:aideo
```

Everything links statically except glibc.

## Usage

```bash
./aideo video.mp4
```

### Options

| Option | Function |
|------|------|
| `-c, --colors <n>` | Color count: 2, 8, 16, 32, 64, 128, 256 |
| `-v, --volume <n>` | Volume, 0 to 100 |
| `-t, --start <time>` | Start position: seconds, mm:ss or hh:mm:ss |
| `-s, --size <mode>` | Picture size: full or half |
| `-p, --profile` | Write stage timings to `aideo_prof.txt` on exit |
| `-h, --help` | Help page |
| `--version` | Version |

### Shortcuts

| Key | Function |
|------|------|
| `q` / `ESC` | Quit |
| `Space` | Pause / resume |
| `j` / `k` | Color count down / up |
| `↑` / `↓` | Volume up / down |
| `Tab` | Show / hide the frame rate label |
| `m` | Mute / unmute |
| `f` | Toggle full and half size |
| `←` / `→` | Seek 5 seconds back / forward |

Terminal resize adapts without interrupting playback.

### tmux

Needs tmux 3.4 or newer built with `--enable-sixel`, and `set -g focus-events on` in `~/.tmux.conf` so the picture repaints when focus returns. Outside tmux no setup is needed.

## libaideo

The rendering engine builds as a static library:

```bash
odin build . -build-mode:static -out:libaideo.a
```

The interface is `aideo.h`, a plain C ABI callable from any language.

```c
#include "aideo.h"

aideo_config cfg = { .fd = 1, .src_w = 1920, .src_h = 1080,
    .out_w = 640, .out_h = 360, .colors = 256, .origin_row = 1, .origin_col = 1 };
aideo_engine *e = aideo_open(&cfg);
aideo_push_yuv(e, y, u, v, stride_y, stride_c);  /* frames in, terminal out */
aideo_close(e);
```

Link `avx2_quant.o` plus the `ffstatic/` and `miniaudio/` archives, see the `aideo.h` header comment.

## Tests

```bash
./aideo citest    # SIMD consistency for quantization and hashing
./aideo cibench   # quantization and encode timings
./aideo cisws     # scaler byte exactness and timings
./aideo ciframe   # single frame pixel diff diagnostics
```
