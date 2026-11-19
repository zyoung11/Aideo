#!/usr/bin/env sh
# Builds a minimal static FFmpeg into ffstatic/ for self-contained binaries.
# Only the demuxers, decoders, parsers and protocols the player needs are
# enabled, so the archives stay small and the final binary links without any
# FFmpeg shared library. AV1 decoding uses the system libdav1d.
set -e
SRC_DIR="${FFMPEG_SRC:-/tmp/ffmpeg-src}"
ZLIB_SRC="${ZLIB_SRC:-/tmp/zlib-src}"
DAV1D_SRC="${DAV1D_SRC:-/tmp/dav1d-src}"
FFSTATIC_DIR="$(cd "$(dirname "$0")" && pwd)/ffstatic"
JOBS="${JOBS:-$(nproc)}"

if [ ! -f "$ZLIB_SRC/configure" ]; then
	echo "cloning zlib source into $ZLIB_SRC"
	rm -rf "$ZLIB_SRC"
	git clone --depth 1 https://github.com/madler/zlib.git "$ZLIB_SRC"
fi

cd "$ZLIB_SRC"
./configure --static >/dev/null
make -j"$JOBS" >/dev/null
mkdir -p "$FFSTATIC_DIR"
cp libz.a "$FFSTATIC_DIR/"

echo "static zlib archive written"

if [ ! -f "$DAV1D_SRC/meson.build" ]; then
	echo "cloning dav1d source into $DAV1D_SRC"
	rm -rf "$DAV1D_SRC"
	git clone --depth 1 https://code.videolan.org/videolan/dav1d.git "$DAV1D_SRC"
fi

cd "$DAV1D_SRC"
if [ ! -f build/build.ninja ]; then
	meson setup build --default-library=static --buildtype=release \
		-Denable_tools=false -Denable_tests=false >/dev/null
fi
ninja -C build >/dev/null
cp build/src/libdav1d.a "$FFSTATIC_DIR/"

echo "static dav1d archive written"

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
	--enable-demuxer=image_png_pipe,image_jpeg_pipe,image_webp_pipe,image_bmp_pipe,image_tiff_pipe,gif,apng,webp_anim \
	--enable-decoder=h264,hevc,vp8,vp9,libdav1d,aac,mp3,flac,opus,vorbis,ac3,eac3,pcm_s16le,pcm_s24le,pcm_f32le \
	--enable-decoder=png,mjpeg,webp,bmp,gif,tiff,apng,webp_anim \
	--enable-parser=h264,hevc,vp8,vp9,av1,aac,mpegaudio,flac,opus,vorbis,ac3 \
	--enable-bsf=h264_mp4toannexb,hevc_mp4toannexb,extract_extradata \
	--enable-zlib \
	--enable-libdav1d

make -j"$JOBS"

mkdir -p "$FFSTATIC_DIR"
cp libavformat/libavformat.a libavcodec/libavcodec.a libavutil/libavutil.a \
	libswscale/libswscale.a libswresample/libswresample.a "$FFSTATIC_DIR/"

echo "static FFmpeg archives written to $FFSTATIC_DIR"
