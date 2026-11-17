package aideo

import "core:c"

AVMEDIA_TYPE_VIDEO :: 0
AVMEDIA_TYPE_AUDIO :: 1

AV_PIX_FMT_YUV420P :: 0
AV_PIX_FMT_RGBA    :: 26

AV_SAMPLE_FMT_S16  :: 1
AV_SAMPLE_FMT_S16P :: 6
AV_SAMPLE_FMT_FLTP :: 8

AVSEEK_FLAG_BACKWARD :: 1

AVERROR_EAGAIN :: c.int(-11)
AVERROR_EOF    :: c.int(-541478725)

AV_LOG_QUIET :: -8

SWS_FAST_BILINEAR :: 1
SWS_BILINEAR      :: 2

AVRational :: struct {
	num, den: c.int,
}

AVChannelLayout :: struct {
	order:        i32,
	nb_channels:  i32,
	u:            u64,
	opaque:       rawptr,
}

AVCodecParameters :: struct {
	codec_type:         i32,
	codec_id:           i32,
	codec_tag:          u32,
	extradata:          [^]u8,
	extradata_size:     c.int,
	coded_side_data:    rawptr,
	nb_coded_side_data: c.int,
	format:             c.int,
	bit_rate:           i64,
	bits_per_coded_sample: c.int,
	bits_per_raw_sample:   c.int,
	profile:            c.int,
	level:              c.int,
	width:              c.int,
	height:             c.int,
	sample_aspect_ratio: AVRational,
	framerate:          AVRational,
	field_order:        i32,
	color_range:        i32,
	color_primaries:    i32,
	color_trc:          i32,
	color_space:        i32,
	chroma_location:    i32,
	video_delay:        c.int,
	ch_layout:          AVChannelLayout,
	sample_rate:        c.int,
	block_align:        c.int,
}

AVStream :: struct {
	av_class:  rawptr,
	index:     c.int,
	id:        c.int,
	codecpar:  ^AVCodecParameters,
	priv_data: rawptr,
	time_base: AVRational,
	start_time: i64,
	duration:   i64,
	nb_frames:  i64,
}

AVFormatContext :: struct {
	av_class:        rawptr,
	iformat:         rawptr,
	oformat:         rawptr,
	priv_data:       rawptr,
	pb:              rawptr,
	ctx_flags:       c.int,
	nb_streams:      u32,
	streams:         [^]^AVStream,
	nb_stream_groups: u32,
	stream_groups:   rawptr,
	nb_chapters:     u32,
	chapters:        rawptr,
	url:             [^]u8,
	start_time:      i64,
	duration:        i64,
}

AVFrame :: struct {
	data:          [8][^]u8,
	linesize:      [8]c.int,
	extended_data: [^][^]u8,
	width:         c.int,
	height:        c.int,
	nb_samples:    c.int,
	format:        c.int,
	pict_type:     i32,
	sample_aspect_ratio: AVRational,
	pts:           i64,
	pkt_dts:       i64,
}

AVPacket :: struct {
	buf:          rawptr,
	pts:          i64,
	dts:          i64,
	data:         [^]u8,
	size:         c.int,
	stream_index: c.int,
	flags:        c.int,
}

AVCodecContext :: struct{}
SwsContext     :: struct{}
SwrContext     :: struct{}
AVCodec        :: struct{}
AVInputFormat  :: struct{}

foreign import libav {
	"ffstatic/libavformat.a",
	"ffstatic/libavcodec.a",
	"ffstatic/libswscale.a",
	"ffstatic/libswresample.a",
	"ffstatic/libavutil.a",
	"system:dav1d",
	"system:z",
	"system:m",
	"system:pthread",
}

foreign libav {
	avformat_open_input :: proc(ps: ^^AVFormatContext, url: cstring, fmt: rawptr, options: rawptr) -> c.int ---
	avformat_close_input :: proc(s: ^^AVFormatContext) ---
	avformat_find_stream_info :: proc(ic: ^AVFormatContext, options: rawptr) -> c.int ---
	av_read_frame :: proc(s: ^AVFormatContext, pkt: ^AVPacket) -> c.int ---
	av_seek_frame :: proc(s: ^AVFormatContext, stream_index: c.int, timestamp: i64, flags: c.int) -> c.int ---
	av_find_best_stream :: proc(ic: ^AVFormatContext, media_type: c.int, wanted_stream_index: c.int, related_stream: c.int, decoder_ret: rawptr, flags: c.int) -> c.int ---
	av_guess_frame_rate :: proc(ic: ^AVFormatContext, stream: ^AVStream, frame: ^AVFrame) -> AVRational ---

	avcodec_find_decoder :: proc(id: c.int) -> ^AVCodec ---
	avcodec_alloc_context3 :: proc(codec: ^AVCodec) -> ^AVCodecContext ---
	avcodec_parameters_to_context :: proc(codec: ^AVCodecContext, par: ^AVCodecParameters) -> c.int ---
	avcodec_open2 :: proc(avctx: ^AVCodecContext, codec: ^AVCodec, options: rawptr) -> c.int ---
	avcodec_free_context :: proc(avctx: ^^AVCodecContext) ---
	avcodec_send_packet :: proc(avctx: ^AVCodecContext, pkt: ^AVPacket) -> c.int ---
	avcodec_receive_frame :: proc(avctx: ^AVCodecContext, frame: ^AVFrame) -> c.int ---

	av_packet_alloc :: proc() -> ^AVPacket ---
	av_packet_free :: proc(pkt: ^^AVPacket) ---
	av_packet_unref :: proc(pkt: ^AVPacket) ---

	av_frame_alloc :: proc() -> ^AVFrame ---
	av_frame_free :: proc(frame: ^^AVFrame) ---
	av_frame_unref :: proc(frame: ^AVFrame) ---
	av_frame_move_ref :: proc(dst: ^AVFrame, src: ^AVFrame) ---

	av_log_set_level :: proc(level: c.int) ---
	avcodec_get_name :: proc(id: c.int) -> cstring ---
	av_opt_set :: proc(obj: rawptr, name: cstring, val: cstring, search_flags: c.int) -> c.int ---
	avcodec_flush_buffers :: proc(avctx: ^AVCodecContext) ---
	av_channel_layout_default :: proc(ch_layout: ^AVChannelLayout, nb_channels: c.int) ---
	av_channel_layout_uninit :: proc(ch_layout: ^AVChannelLayout) ---

	sws_getContext :: proc(srcW: c.int, srcH: c.int, srcFormat: c.int, dstW: c.int, dstH: c.int, dstFormat: c.int, flags: c.int, srcFilter: rawptr, dstFilter: rawptr, param: rawptr) -> ^SwsContext ---
	sws_scale :: proc(swc: ^SwsContext, srcData: rawptr, srcSlice: rawptr, srcSliceY: c.int, srcSliceH: c.int, dstData: rawptr, dstStride: rawptr) -> c.int ---
	sws_freeContext :: proc(c: ^SwsContext) ---

	swr_alloc_set_opts2 :: proc(ps: ^^SwrContext, out_ch_layout: ^AVChannelLayout, out_sample_fmt: c.int, out_sample_rate: c.int, in_ch_layout: ^AVChannelLayout, in_sample_fmt: c.int, in_sample_rate: c.int, log_offset: c.int, log_ctx: rawptr) -> c.int ---
	swr_init :: proc(s: ^SwrContext) -> c.int ---
	swr_convert :: proc(s: ^SwrContext, out: rawptr, out_count: c.int, in_data: rawptr, in_count: c.int) -> c.int ---
	swr_get_delay :: proc(s: ^SwrContext, base: i64) -> i64 ---
	swr_free :: proc(s: ^^SwrContext) ---
}
