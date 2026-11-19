package aideo

import "core:c"
import "core:fmt"
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
}

fq_init :: proc(q: ^FrameQueue) {
	sync.sema_post(&q.free_sem, len(q.slots))
}

// fq_push appends one frame, blocking while the queue is full. It
// returns false when stop is set before space becomes available.
fq_push :: proc(q: ^FrameQueue, m: FrameMsg, stop: ^i32) -> bool {
	for {
		if sync.atomic_load(stop) != 0 {
			return false
		}
		if sync.sema_wait_with_timeout(&q.free_sem, 10 * time.Millisecond) {
			break
		}
	}
	sync.mutex_lock(&q.mu)
	q.slots[(q.head + q.count) % len(q.slots)] = m
	q.count += 1
	sync.mutex_unlock(&q.mu)
	return true
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

VideoStream :: struct {
	fmt_ctx:    ^AVFormatContext,
	codec_ctx:  ^AVCodecContext,
	codec_name: cstring,
	path:       cstring,
	sws:        ^SwsContext,
	retired:    [dynamic]^SwsContext,
	plan:       ^ScalePlan,
	plan_old:   [dynamic]^ScalePlan,
	pkt:        ^AVPacket,
	frame:      ^AVFrame,
	stream_idx: int,
	time_base:  f64,
	duration_sec: f64,
	fps:        f64,
	src_w:      int,
	src_h:      int,
	src_format: int,
	loop_forever: bool,
}

VideoPlayer :: struct {
	vs:          ^VideoStream,
	queue:       FrameQueue,
	yuvq:        YuvQueue,
	spool:       ScalePool,
	sws_mu:      sync.Mutex,
	pool:        [3][]u8,
	pool_mu:     sync.Mutex,
	out_w:       int,
	out_h:       int,
	req_w:       i64,
	req_h:       i64,
	req_resize:  i32,
	req_seek:    i32,
	req_seek_ns: i64,
	stop:        i32,
	loop_offset: i64,
	last_pts:    i64,
	exited:      sync.Sema,
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
	threads_opt := strings_clone_to_cstring(fmt.tprintf("%d", max(cpu_count() / 2, 2)))
	av_opt_set(codec_ctx, "threads", threads_opt, 0)
	if avcodec_open2(codec_ctx, codec, nil) < 0 {
		avcodec_free_context(&codec_ctx)
		avformat_close_input(&fmt_ctx)
		return nil
	}
	vs := new(VideoStream)
	vs.fmt_ctx = fmt_ctx
	vs.codec_ctx = codec_ctx
	vs.codec_name = avcodec_get_name(par.codec_id)
	vs.path = path
	vs.pkt = av_packet_alloc()
	vs.frame = av_frame_alloc()
	vs.stream_idx = int(idx)
	tb := fmt_ctx.streams[idx].time_base
	vs.time_base = f64(tb.num) / f64(tb.den)
	gfr := av_guess_frame_rate(fmt_ctx, fmt_ctx.streams[idx], nil)
	if gfr.num != 0 && gfr.den != 0 {
		vs.fps = f64(gfr.num) / f64(gfr.den)
	}
	// vs.duration_sec picks the first known value of container duration,
	// stream duration and frame count over frame rate. Zero means the
	// total length is unknown and playback stops at EOF instead of looping.
	if fmt_ctx.duration != AV_NOPTS_VALUE && fmt_ctx.duration > 0 {
		vs.duration_sec = f64(fmt_ctx.duration) / 1000000.0
	} else {
		sd := fmt_ctx.streams[idx].duration
		if sd != AV_NOPTS_VALUE && sd > 0 {
			vs.duration_sec = f64(sd) * vs.time_base
		} else if fmt_ctx.streams[idx].nb_frames > 0 && vs.fps > 0 {
			vs.duration_sec = f64(fmt_ctx.streams[idx].nb_frames) / vs.fps
		}
	}
	vs.src_w = int(par.width)
	vs.src_h = int(par.height)
	return vs
}

// video_close_stream releases the decoder side resources of one stream.
video_close_stream :: proc(vs: ^VideoStream) {
	if vs.sws != nil {
		sws_freeContext(vs.sws)
	}
	for ctx in vs.retired {
		sws_freeContext(ctx)
	}
	for p in vs.plan_old {
		scale_plan_free(p)
	}
	av_packet_free(&vs.pkt)
	av_frame_free(&vs.frame)
	avcodec_free_context(&vs.codec_ctx)
	avformat_close_input(&vs.fmt_ctx)
	free(vs)
}

// video_reopen_stream reopens the container and decoder from the start
// of the file. Image based demuxers cannot seek, so looping a still
// animation needs a full reopen instead of av_seek_frame.
video_reopen_stream :: proc(vs: ^VideoStream) -> bool {
	avcodec_free_context(&vs.codec_ctx)
	avformat_close_input(&vs.fmt_ctx)
	fmt_ctx: ^AVFormatContext
	if avformat_open_input(&fmt_ctx, vs.path, nil, nil) != 0 {
		return false
	}
	if avformat_find_stream_info(fmt_ctx, nil) < 0 {
		avformat_close_input(&fmt_ctx)
		return false
	}
	idx := av_find_best_stream(fmt_ctx, AVMEDIA_TYPE_VIDEO, -1, -1, nil, 0)
	if idx < 0 {
		avformat_close_input(&fmt_ctx)
		return false
	}
	par := fmt_ctx.streams[idx].codecpar
	codec := avcodec_find_decoder(par.codec_id)
	if codec == nil {
		avformat_close_input(&fmt_ctx)
		return false
	}
	codec_ctx := avcodec_alloc_context3(codec)
	if avcodec_parameters_to_context(codec_ctx, par) < 0 {
		avcodec_free_context(&codec_ctx)
		avformat_close_input(&fmt_ctx)
		return false
	}
	threads_opt := strings_clone_to_cstring(fmt.tprintf("%d", max(cpu_count() / 2, 2)))
	av_opt_set(codec_ctx, "threads", threads_opt, 0)
	if avcodec_open2(codec_ctx, codec, nil) < 0 {
		avcodec_free_context(&codec_ctx)
		avformat_close_input(&fmt_ctx)
		return false
	}
	vs.fmt_ctx = fmt_ctx
	vs.codec_ctx = codec_ctx
	vs.stream_idx = int(idx)
	return true
}

// video_make_sws rebuilds the pixel scaler for a new output size. The
// old scaler is retired instead of freed because the scale thread may
// still hold it. Callers must hold vp.sws_mu.
video_make_sws :: proc(vs: ^VideoStream, out_w, out_h: int) {
	if vs.sws != nil {
		append(&vs.retired, vs.sws)
	}
	if vs.plan != nil {
		append(&vs.plan_old, vs.plan)
	}
	vs.sws = sws_getContext(c.int(vs.src_w), c.int(vs.src_h), c.int(vs.src_format),
		c.int(out_w), c.int(out_h), AV_PIX_FMT_RGBA, SWS_FAST_BILINEAR, nil, nil, nil)
	vs.plan = scale_plan(vs.src_w, vs.src_h, out_w, out_h)
}

// YuvQueue carries decoded source frames from the decode thread to the
// scale thread. frame.pts holds absolute presentation time in nanoseconds,
// computed by the decode thread at decode time.
YuvQueue :: struct {
	slots:  [4]^AVFrame,
	head:   int,
	count:  int,
	mu:     sync.Mutex,
	used:   sync.Sema,
	free:   sync.Sema,
	yfree:  sync.Sema,
	poolmu: sync.Mutex,
	pool:   [5]^AVFrame,
}

yuv_init :: proc(q: ^YuvQueue) {
	for i in 0..<len(q.pool) {
		q.pool[i] = av_frame_alloc()
	}
	sync.sema_post(&q.free, len(q.slots))
	sync.sema_post(&q.yfree, len(q.pool))
}

// yuv_get_frame takes a free source frame from the pool. It returns nil
// once stop is set so the decode thread can exit at any wait point.
yuv_get_frame :: proc(q: ^YuvQueue, stop: ^i32) -> ^AVFrame {
	for {
		if sync.atomic_load(stop) != 0 {
			return nil
		}
		if sync.sema_wait_with_timeout(&q.yfree, 10 * time.Millisecond) {
			break
		}
	}
	sync.mutex_lock(&q.poolmu)
	for i in 0..<len(q.pool) {
		if q.pool[i] != nil {
			f := q.pool[i]
			q.pool[i] = nil
			sync.mutex_unlock(&q.poolmu)
			return f
		}
	}
	sync.mutex_unlock(&q.poolmu)
	return nil
}

yuv_put_frame :: proc(q: ^YuvQueue, f: ^AVFrame) {
	av_frame_unref(f)
	sync.mutex_lock(&q.poolmu)
	for i := len(q.pool) - 1; i >= 0; i -= 1 {
		if q.pool[i] == nil {
			q.pool[i] = f
			sync.mutex_unlock(&q.poolmu)
			sync.sema_post(&q.yfree)
			return
		}
	}
	sync.mutex_unlock(&q.poolmu)
}

// yuv_drain returns every queued source frame to the pool so stale
// frames from before a seek or a resize never reach the scaler.
yuv_drain :: proc(q: ^YuvQueue) {
	for {
		sync.mutex_lock(&q.mu)
		if q.count == 0 {
			sync.mutex_unlock(&q.mu)
			return
		}
		f := q.slots[q.head]
		q.head = (q.head + 1) % len(q.slots)
		q.count -= 1
		sync.sema_post(&q.free)
		sync.mutex_unlock(&q.mu)
		yuv_put_frame(q, f)
	}
}

// yuv_push hands one decoded frame to the scale thread. It returns false
// when stop is set first, leaving ownership with the caller.
yuv_push :: proc(q: ^YuvQueue, f: ^AVFrame, stop: ^i32) -> bool {
	for {
		if sync.atomic_load(stop) != 0 {
			return false
		}
		if sync.sema_wait_with_timeout(&q.free, 10 * time.Millisecond) {
			break
		}
	}
	sync.mutex_lock(&q.mu)
	q.slots[(q.head + q.count) % len(q.slots)] = f
	q.count += 1
	sync.mutex_unlock(&q.mu)
	sync.sema_post(&q.used)
	return true
}

// yuv_pop takes one decoded frame. It returns nil once stop is set so
// the scale thread can exit at any wait point. A wake up against an empty
// queue means the signal is stale after a drain and is discarded.
yuv_pop :: proc(q: ^YuvQueue, stop: ^i32) -> ^AVFrame {
	for {
		if sync.atomic_load(stop) != 0 {
			return nil
		}
		if !sync.sema_wait_with_timeout(&q.used, 10 * time.Millisecond) {
			continue
		}
		sync.mutex_lock(&q.mu)
		if q.count == 0 {
			sync.mutex_unlock(&q.mu)
			continue
		}
		f := q.slots[q.head]
		q.head = (q.head + 1) % len(q.slots)
		q.count -= 1
		sync.mutex_unlock(&q.mu)
		sync.sema_post(&q.free)
		return f
	}
}

// video_take_frame moves one decoded frame into the scaling queue with
// its absolute presentation time.
video_take_frame :: proc(vp: ^VideoPlayer, vs: ^VideoStream) {
	sync.mutex_lock(&vp.sws_mu)
	if vs.sws == nil {
		vs.src_format = int(vs.frame.format)
		video_make_sws(vs, vp.out_w, vp.out_h)
	}
	sync.mutex_unlock(&vp.sws_mu)
	yf := yuv_get_frame(&vp.yuvq, &vp.stop)
	if yf == nil {
		return
	}
	av_frame_move_ref(yf, vs.frame)
	pts_ns := sync.atomic_load(&vp.loop_offset)
	if yf.pts != min(i64) {
		pts_ns += i64(f64(yf.pts) * vs.time_base * 1000000000.0)
	}
	yf.pts = pts_ns
	sync.atomic_store(&vp.last_pts, pts_ns)
	if !yuv_push(&vp.yuvq, yf, &vp.stop) {
		yuv_put_frame(&vp.yuvq, yf)
	}
}

video_scale_thread :: proc(vp: ^VideoPlayer) {
	defer sync.sema_post(&vp.exited)
	vs := vp.vs
	for sync.atomic_load(&vp.stop) == 0 {
		yf := yuv_pop(&vp.yuvq, &vp.stop)
		if yf == nil {
			break
		}
		_t2 := time.now()
		sync.mutex_lock(&vp.sws_mu)
		sws := vs.sws
		plan := vs.plan
		ow := vp.out_w
		oh := vp.out_h
		sync.mutex_unlock(&vp.sws_mu)
		buf: []u8
		if sws != nil {
			fs := ow * oh * 4
			buf = video_pool_get(vp, fs)
			if int(yf.format) == AV_PIX_FMT_YUV420P && plan != nil {
				scale_frame(&vp.spool, plan, yf.data[0], yf.data[1], yf.data[2],
					int(yf.linesize[0]), int(yf.linesize[1]), &buf[0], ow * 4)
			} else {
				dst_data: [1][^]u8
				dst_data[0] = ([^]u8)(&buf[0])
				dst_stride: [1]c.int
				dst_stride[0] = c.int(ow * 4)
				sws_scale(sws, &yf.data[0], &yf.linesize[0], 0, yf.height, &dst_data[0], &dst_stride[0])
			}
		}
		prof_add(PROF_DECODE_SWS, i64(time.since(_t2)))
		pts_sec := f64(yf.pts) / 1000000000.0
		yuv_put_frame(&vp.yuvq, yf)
		if len(buf) > 0 {
			if !fq_push(&vp.queue, FrameMsg{buf = buf, pts_sec = pts_sec}, &vp.stop) {
				video_pool_put(vp, buf)
			}
		}
	}
}

video_decode_thread :: proc(vp: ^VideoPlayer) {
	defer sync.sema_post(&vp.exited)
	vs := vp.vs

	for sync.atomic_load(&vp.stop) == 0 {
		if sync.atomic_load(&vp.req_seek) != 0 {
			tgt := sync.atomic_load(&vp.req_seek_ns)
			sync.atomic_store(&vp.req_seek, 0)
			loff := sync.atomic_load(&vp.loop_offset)
			seek_target := i64(f64(tgt - loff) / 1e9 / vs.time_base)
			if seek_target < 0 {
				seek_target = 0
			}
			av_seek_frame(vs.fmt_ctx, c.int(vs.stream_idx), seek_target, AVSEEK_FLAG_BACKWARD)
			avcodec_flush_buffers(vs.codec_ctx)
			yuv_drain(&vp.yuvq)
			for {
				m, ok := fq_try_pop(&vp.queue)
				if !ok {
					break
				}
				video_pool_put(vp, m.buf)
			}
			continue
		}

		if sync.atomic_load(&vp.req_resize) != 0 {
			nw := int(sync.atomic_load(&vp.req_w))
			nh := int(sync.atomic_load(&vp.req_h))
			sync.atomic_store(&vp.req_resize, 0)
			sync.mutex_lock(&vp.sws_mu)
			vp.out_w = nw
			vp.out_h = nh
			video_make_sws(vs, nw, nh)
			sync.mutex_unlock(&vp.sws_mu)
			yuv_drain(&vp.yuvq)
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
			avcodec_send_packet(vs.codec_ctx, nil)
			for {
				if avcodec_receive_frame(vs.codec_ctx, vs.frame) < 0 {
					break
				}
				video_take_frame(vp, vs)
			}
			period := i64(vs.duration_sec * 1000000000.0)
			if period <= 0 {
				if !vs.loop_forever {
					sync.atomic_store(&vp.stop, 1)
					break
				}
				last := sync.atomic_load(&vp.last_pts)
				period = last - sync.atomic_load(&vp.loop_offset)
				if period <= 0 {
					period = 1
				}
			}
			if vs.loop_forever {
				if !video_reopen_stream(vs) {
					sync.atomic_store(&vp.stop, 1)
					break
				}
			} else if av_seek_frame(vs.fmt_ctx, c.int(vs.stream_idx), 0, AVSEEK_FLAG_BACKWARD) < 0 {
				if !video_reopen_stream(vs) {
					sync.atomic_store(&vp.stop, 1)
					break
				}
			}
			avcodec_flush_buffers(vs.codec_ctx)
			sync.atomic_store(&vp.loop_offset, sync.atomic_load(&vp.loop_offset) + period)
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
		for {
			sres := avcodec_send_packet(vs.codec_ctx, vs.pkt)
			if sres != AVERROR_EAGAIN {
				break
			}
			if avcodec_receive_frame(vs.codec_ctx, vs.frame) < 0 {
				break
			}
			prof_add(PROF_DECODE_RECV, i64(time.since(_t1)))
			video_take_frame(vp, vs)
		}
		av_packet_unref(vs.pkt)
		for {
			if avcodec_receive_frame(vs.codec_ctx, vs.frame) < 0 {
				break
			}
			prof_add(PROF_DECODE_RECV, i64(time.since(_t1)))
			video_take_frame(vp, vs)
		}
	}
}

video_start :: proc(vp: ^VideoPlayer) {
	fq_init(&vp.queue)
	yuv_init(&vp.yuvq)
	scale_pool_init(&vp.spool, max(cpu_count() / 2, 2))
	thread.create_and_start_with_data(vp, proc(data: rawptr) {
		video_scale_thread((^VideoPlayer)(data))
	}, nil, thread.Thread_Priority.Normal, true)
	for i in 0..<len(vp.pool) {
		vp.pool[i] = make([]u8, vp.out_w * vp.out_h * 4)
	}
	thread.create_and_start_with_data(vp, proc(data: rawptr) {
		video_decode_thread((^VideoPlayer)(data))
	})
}

// video_request_seek asks the decode thread to jump to an absolute
// timeline position in nanoseconds.
video_request_seek :: proc(vp: ^VideoPlayer, abs_ns: i64) {
	sync.atomic_store(&vp.req_seek_ns, abs_ns)
	sync.atomic_store(&vp.req_seek, 1)
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

// video_stop stops the decode and scale threads and waits for both to
// exit before shutting the scaling workers down.
video_stop :: proc(vp: ^VideoPlayer) {
	sync.atomic_store(&vp.stop, 1)
	sync.sema_wait(&vp.exited)
	sync.sema_wait(&vp.exited)
	scale_pool_stop(&vp.spool)
}

// video_close releases every resource owned by the player. Callers must
// call video_stop first so no thread touches these resources.
video_close :: proc(vp: ^VideoPlayer) {
	for {
		m, ok := fq_try_pop(&vp.queue)
		if !ok {
			break
		}
		video_pool_put(vp, m.buf)
	}
	sync.mutex_lock(&vp.pool_mu)
	for i in 0..<len(vp.pool) {
		if vp.pool[i] != nil {
			free(&vp.pool[i][0])
			vp.pool[i] = nil
		}
	}
	sync.mutex_unlock(&vp.pool_mu)
	video_close_stream(vp.vs)
}
