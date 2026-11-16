package aideo

import "core:c"
import "core:sync"
import "core:time"
import "core:thread"

FrameMsg :: struct {
	buf:    []u8,
	pts_sec: f64,
}

FrameQueue :: struct {
	mu:       sync.Mutex,
	slots:    [4]FrameMsg,
	head:     int,
	count:    int,
	free_sem: sync.Sema,
	used_sem: sync.Sema,
}

fq_init :: proc(q: ^FrameQueue) {
	sync.sema_post(&q.free_sem, len(q.slots))
}

fq_push :: proc(q: ^FrameQueue, m: FrameMsg) {
	sync.sema_wait(&q.free_sem)
	sync.mutex_lock(&q.mu)
	q.slots[(q.head + q.count) % len(q.slots)] = m
	q.count += 1
	sync.mutex_unlock(&q.mu)
	sync.sema_post(&q.used_sem)
}

fq_pop :: proc(q: ^FrameQueue) -> FrameMsg {
	sync.sema_wait(&q.used_sem)
	sync.mutex_lock(&q.mu)
	m := q.slots[q.head]
	q.head = (q.head + 1) % len(q.slots)
	q.count -= 1
	sync.mutex_unlock(&q.mu)
	sync.sema_post(&q.free_sem)
	return m
}

fq_try_pop :: proc(q: ^FrameQueue) -> (FrameMsg, bool) {
	sync.mutex_lock(&q.mu)
	if q.count == 0 {
		sync.mutex_unlock(&q.mu)
		return {}, false
	}
	m := q.slots[q.head]
	q.head = (q.head + 1) % len(q.slots)
	q.count -= 1
	sync.mutex_unlock(&q.mu)
	sync.sema_post(&q.free_sem)
	return m, true
}

fq_drain :: proc(q: ^FrameQueue) {
	for {
		_, ok := fq_try_pop(q)
		if !ok {
			return
		}
	}
}

VideoStream :: struct {
	fmt_ctx:    ^AVFormatContext,
	codec_ctx:  ^AVCodecContext,
	sws:        ^SwsContext,
	pkt:        ^AVPacket,
	frame:      ^AVFrame,
	stream_idx: int,
	time_base:  f64,
	duration_sec: f64,
	src_w:      int,
	src_h:      int,
	src_format: int,
}

VideoPlayer :: struct {
	vs:          ^VideoStream,
	queue:       FrameQueue,
	pool:        [3][]u8,
	pool_mu:     sync.Mutex,
	out_w:       int,
	out_h:       int,
	req_w:       i64,
	req_h:       i64,
	req_resize:  i32,
	stop:        i32,
	loop_offset: i64,
	last_pts:    i64,
	has_audio:   bool,
}

video_pool_get :: proc(vp: ^VideoPlayer, fs: int) -> []u8 {
	sync.mutex_lock(&vp.pool_mu)
	for i := len(vp.pool) - 1; i >= 0; i -= 1 {
		if vp.pool[i] != nil {
			b := vp.pool[i]
			vp.pool[i] = nil
			sync.mutex_unlock(&vp.pool_mu)
			if len(b) != fs {
				free(&b[0])
				b = make([]u8, fs)
			}
			return b
		}
	}
	sync.mutex_unlock(&vp.pool_mu)
	return make([]u8, fs)
}

video_pool_put :: proc(vp: ^VideoPlayer, b: []u8) {
	sync.mutex_lock(&vp.pool_mu)
	for i in 0..<len(vp.pool) {
		if vp.pool[i] == nil {
			vp.pool[i] = b
			sync.mutex_unlock(&vp.pool_mu)
			return
		}
	}
	sync.mutex_unlock(&vp.pool_mu)
	free(&b[0])
}

video_release_msg :: proc(vp: ^VideoPlayer, m: FrameMsg) {
	video_pool_put(vp, m.buf)
}

video_open :: proc(path: cstring) -> ^VideoStream {
	fmt_ctx: ^AVFormatContext
	if avformat_open_input(&fmt_ctx, path, nil, nil) != 0 {
		return nil
	}
	if avformat_find_stream_info(fmt_ctx, nil) < 0 {
		avformat_close_input(&fmt_ctx)
		return nil
	}
	idx := av_find_best_stream(fmt_ctx, AVMEDIA_TYPE_VIDEO, -1, -1, nil, 0)
	if idx < 0 {
		avformat_close_input(&fmt_ctx)
		return nil
	}
	par := fmt_ctx.streams[idx].codecpar
	codec := avcodec_find_decoder(par.codec_id)
	if codec == nil {
		avformat_close_input(&fmt_ctx)
		return nil
	}
	codec_ctx := avcodec_alloc_context3(codec)
	if avcodec_parameters_to_context(codec_ctx, par) < 0 {
		avcodec_free_context(&codec_ctx)
		avformat_close_input(&fmt_ctx)
		return nil
	}
	codec_name := string(avcodec_get_name(par.codec_id))
	if codec_name == "libdav1d" || codec_name == "av1" {
		av_opt_set(codec_ctx, "threads", "0", 0)
	} else {
		av_opt_set(codec_ctx, "threads", "2", 0)
	}
	if avcodec_open2(codec_ctx, codec, nil) < 0 {
		avcodec_free_context(&codec_ctx)
		avformat_close_input(&fmt_ctx)
		return nil
	}
	vs := new(VideoStream)
	vs.fmt_ctx = fmt_ctx
	vs.codec_ctx = codec_ctx
	vs.pkt = av_packet_alloc()
	vs.frame = av_frame_alloc()
	vs.stream_idx = int(idx)
	tb := fmt_ctx.streams[idx].time_base
	vs.time_base = f64(tb.num) / f64(tb.den)
	vs.duration_sec = f64(fmt_ctx.duration) / 1000000.0
	vs.src_w = int(par.width)
	vs.src_h = int(par.height)
	return vs
}

video_close :: proc(vs: ^VideoStream) {
	if vs.sws != nil {
		sws_freeContext(vs.sws)
	}
	av_packet_free(&vs.pkt)
	av_frame_free(&vs.frame)
	avcodec_free_context(&vs.codec_ctx)
	avformat_close_input(&vs.fmt_ctx)
	free(vs)
}

video_make_sws :: proc(vs: ^VideoStream, out_w, out_h: int) {
	if vs.sws != nil {
		sws_freeContext(vs.sws)
	}
	vs.sws = sws_getContext(c.int(vs.src_w), c.int(vs.src_h), c.int(vs.src_format),
		c.int(out_w), c.int(out_h), AV_PIX_FMT_RGBA, SWS_FAST_BILINEAR, nil, nil, nil)
}

video_decode_thread :: proc(vp: ^VideoPlayer) {
	vs := vp.vs

	for sync.atomic_load(&vp.stop) == 0 {
		if sync.atomic_load(&vp.req_resize) != 0 {
			nw := int(sync.atomic_load(&vp.req_w))
			nh := int(sync.atomic_load(&vp.req_h))
			sync.atomic_store(&vp.req_resize, 0)
			vp.out_w = nw
			vp.out_h = nh
			video_make_sws(vs, nw, nh)
			cur := f64(sync.atomic_load(&vp.last_pts)) / 1000000000.0
			seek_target := i64((cur - f64(sync.atomic_load(&vp.loop_offset)) / 1000000000.0) / vs.time_base)
			av_seek_frame(vs.fmt_ctx, c.int(vs.stream_idx), seek_target, AVSEEK_FLAG_BACKWARD)
			avcodec_flush_buffers(vs.codec_ctx)
			for {
				m, ok := fq_try_pop(&vp.queue)
				if !ok {
					break
				}
				video_pool_put(vp, m.buf)
			}
			continue
		}

		_t0 := time.now()
		res := av_read_frame(vs.fmt_ctx, vs.pkt)
		prof_add(PROF_DECODE_READ, i64(time.since(_t0)))
		if res == AVERROR_EOF {
			av_seek_frame(vs.fmt_ctx, c.int(vs.stream_idx), 0, AVSEEK_FLAG_BACKWARD)
			avcodec_flush_buffers(vs.codec_ctx)
			new_offset := sync.atomic_load(&vp.loop_offset) + i64(vs.duration_sec * 1000000000.0)
			sync.atomic_store(&vp.loop_offset, new_offset)
			continue
		}
		if res < 0 {
			return
		}
		if int(vs.pkt.stream_index) != vs.stream_idx {
			av_packet_unref(vs.pkt)
			continue
		}
		_t1 := time.now()
		if avcodec_send_packet(vs.codec_ctx, vs.pkt) < 0 {
			av_packet_unref(vs.pkt)
			continue
		}
		av_packet_unref(vs.pkt)
		for {
			if avcodec_receive_frame(vs.codec_ctx, vs.frame) < 0 {
				break
			}
			if vs.sws == nil {
				vs.src_format = int(vs.frame.format)
				video_make_sws(vs, vp.out_w, vp.out_h)
			}
			fs := vp.out_w * vp.out_h * 4
			buf := video_pool_get(vp, fs)
			if buf == nil {
				av_frame_unref(vs.frame)
				break
			}
			out_w, out_h := vp.out_w, vp.out_h
			dst_data: [1][^]u8
			dst_data[0] = ([^]u8)(&buf[0])
			prof_add(PROF_DECODE_RECV, i64(time.since(_t1)))
			_t2 := time.now()
			dst_stride: [1]c.int
			dst_stride[0] = c.int(out_w * 4)
			sws_scale(vs.sws, &vs.frame.data[0], &vs.frame.linesize[0], 0, vs.frame.height, &dst_data[0], &dst_stride[0])
			prof_add(PROF_DECODE_SWS, i64(time.since(_t2)))

			pts_ns := sync.atomic_load(&vp.loop_offset)
			if vs.frame.pts != min(i64) {
				pts_ns += i64(f64(vs.frame.pts) * vs.time_base * 1000000000.0)
			}
			av_frame_unref(vs.frame)
			sync.atomic_store(&vp.last_pts, pts_ns)
			fq_push(&vp.queue, FrameMsg{buf = buf, pts_sec = f64(pts_ns) / 1000000000.0})
		}
	}
}

video_start :: proc(vp: ^VideoPlayer) {
	fq_init(&vp.queue)
	for i in 0..<len(vp.pool) {
		vp.pool[i] = make([]u8, vp.out_w * vp.out_h * 4)
	}
	thread.create_and_start_with_data(vp, proc(data: rawptr) {
		video_decode_thread((^VideoPlayer)(data))
	})
}

video_request_resize :: proc(vp: ^VideoPlayer, w, h: int) {
	sync.mutex_lock(&vp.pool_mu)
	for i in 0..<len(vp.pool) {
		if vp.pool[i] != nil {
			free(&vp.pool[i][0])
			vp.pool[i] = nil
		}
	}
	sync.mutex_unlock(&vp.pool_mu)
	sync.atomic_store(&vp.req_w, i64(w))
	sync.atomic_store(&vp.req_h, i64(h))
	sync.atomic_store(&vp.req_resize, 1)
}

video_stop :: proc(vp: ^VideoPlayer) {
	sync.atomic_store(&vp.stop, 1)
}
