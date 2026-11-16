#!/usr/bin/env sh
# Builds a minimal static FFmpeg into ffstatic/ for self-contained binaries.
# Only the demuxers, decoders, parsers and protocols the player needs are
# enabled, so the archives stay small and the final binary links without any
# FFmpeg shared library.
set -e

SRC_DIR="${FFMPEG_SRC:-/tmp/ffmpeg-src}"
FFSTATIC_DIR="$(cd "$(dirname "$0")" && pwd)/ffstatic"
JOBS="${JOBS:-$(nproc)}"

if [ ! -f "$SRC_DIR/configure" ]; then
	echo "cloning FFmpeg source into $SRC_DIR"
	rm -rf "$SRC_DIR"
	git clone --depth 1 https://github.com/FFmpeg/FFmpeg.git "$SRC_DIR"
fi

cd "$SRC_DIR"

./configure \
	--prefix=/opt/ffstatic \
	--disable-shared --enable-static \
	--disable-debug --disable-doc --disable-programs \
	--enable-small --disable-autodetect --disable-everything \
	--enable-avformat --enable-avcodec --enable-swresample --enable-swscale --enable-avutil \
	--enable-protocol=file,pipe \
	--enable-demuxer=mov,matroska,avi,mpegts,ogg,wav,flac,mp3,flv \
	--enable-decoder=h264,hevc,vp8,vp9,av1,aac,mp3,flac,opus,vorbis,ac3,eac3,pcm_s16le,pcm_s24le,pcm_f32le \
	--enable-parser=h264,hevc,vp8,vp9,av1,aac,mpegaudio,flac,opus,vorbis,ac3 \
	--enable-bsf=h264_mp4toannexb,hevc_mp4toannexb,extract_extradata

make -j"$JOBS"

mkdir -p "$FFSTATIC_DIR"
cp libavformat/libavformat.a libavcodec/libavcodec.a libavutil/libavutil.a \
	libswscale/libswscale.a libswresample/libswresample.a "$FFSTATIC_DIR/"

echo "static FFmpeg archives written to $FFSTATIC_DIR"
echo "now run: odin build . -o:speed"
