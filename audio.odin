package aideo

import "core:c"
import "core:sync"
import "core:thread"
import "core:time"
import ma "miniaudio"

AUDIO_RATE     :: 44100
AUDIO_CHANNELS :: 2

RingBuffer :: struct {
	data:    []i16,
	cap:     int,
	head:    i64,
	tail:    i64,
}

ring_init :: proc(rb: ^RingBuffer, frames: int) {
	rb.cap = frames * AUDIO_CHANNELS
	rb.data = make([]i16, rb.cap)
	rb.head = 0
	rb.tail = 0
}

// ring_write copies samples into the ring. Callers must hold audio_state.mu.
ring_write :: proc "contextless" (rb: ^RingBuffer, samples: []i16) -> int {
	head := rb.head
	tail := rb.tail
	space := rb.cap - int(head - tail)
	n := min(len(samples), space)
	if n <= 0 {
		return 0
	}
	pos := int(head % i64(rb.cap))
	first := min(n, rb.cap - pos)
	copy(rb.data[pos:pos+first], samples[:first])
	copy(rb.data[0:n-first], samples[first:n])
	rb.head = head + i64(n)
	return n
}

// ring_read copies samples out of the ring. Callers must hold
// audio_state.mu. A short result means the ring drained and the tail of
// dst is zero filled.
ring_read :: proc "contextless" (rb: ^RingBuffer, dst: []i16) -> int {
	head := rb.head
	tail := rb.tail
	avail := int(head - tail)
	n := min(len(dst), avail)
	if n <= 0 {
		for i in 0..<len(dst) {
			dst[i] = 0
		}
		return 0
	}
	pos := int(tail % i64(rb.cap))
	first := min(n, rb.cap - pos)
	copy(dst[:first], rb.data[pos:pos+first])
	copy(dst[first:n], rb.data[0:n-first])
	rb.tail = tail + i64(n)
	return n
}

AudioState :: struct {
	ctx:     ma.context_type,
	device:  ma.device,
	ring:    RingBuffer,
	consumed: i64,
	paused:  i32,
	volume:  i64,
	scratch: []i16,
	dev_ok:  bool,
	space:   sync.Sema,
	anchor_consumed: i64,
	anchor_wall_ns:  i64,
	epoch:   time.Time,
	seek_req:  i64,
	seek_abs:  i64,
	seek_flag: i32,
	stop:      i32,
	exited:    sync.Sema,
	stream:    ^AudioStream,
	mu:        sync.Mutex,
}

audio_state: AudioState

// audio_data_callback feeds the sound card. Long pulls are processed in
// scratch sized chunks so the output buffer can never overrun scratch.
audio_data_callback :: proc "c" (_: ^ma.device, p_output, p_input: rawptr, frame_count: u32) {
	out := ([^]i16)(p_output)
	total := int(frame_count) * AUDIO_CHANNELS
	if sync.atomic_load(&audio_state.paused) != 0 {
		for i in 0..<total {
			out[i] = 0
		}
		return
	}
	vol_bits := sync.atomic_load(&audio_state.volume)
	vol := transmute(f32)u32(vol_bits)
	sync.mutex_lock(&audio_state.mu)
	audio_state.consumed += i64(frame_count)
	audio_state.anchor_consumed = audio_state.consumed
	audio_state.anchor_wall_ns = i64(time.since(audio_state.epoch))
	sync.mutex_unlock(&audio_state.mu)
	pos := 0
	for pos < total {
		chunk := min(total - pos, len(audio_state.scratch))
		sync.mutex_lock(&audio_state.mu)
		read := ring_read(&audio_state.ring, audio_state.scratch[:chunk])
		sync.mutex_unlock(&audio_state.mu)
		for i in 0..<read {
			s := f32(audio_state.scratch[i]) * vol
			if s > 32767 {
				s = 32767
			} else if s < -32768 {
				s = -32768
			}
			out[pos + i] = i16(s)
		}
		for i in read..<chunk {
			out[pos + i] = 0
		}
		pos += chunk
	}
	sync.sema_post(&audio_state.space)
}

audio_init :: proc() -> bool {
	config := ma.device_config_init(.playback)
	config.playback.format = .s16
	config.playback.channels = AUDIO_CHANNELS
	config.sampleRate = AUDIO_RATE
	config.dataCallback = audio_data_callback
	config.periodSizeInMilliseconds = 50
	ring_init(&audio_state.ring, AUDIO_RATE * 4)
	audio_state.scratch = make([]i16, AUDIO_RATE / 10 * AUDIO_CHANNELS)
	audio_state.epoch = time.now()
	sync.atomic_store(&audio_state.volume, transmute(i64)u64(u32(transmute(u32)f32(1.0))))
	config.pulse.pStreamNamePlayback = cstring("Aideo")
	ctx_config := ma.context_config_init()
	ctx_config.pulse.pApplicationName = cstring("Aideo")
	if ma.context_init(nil, 0, &ctx_config, &audio_state.ctx) != .SUCCESS {
		return false
	}
	if ma.device_init(&audio_state.ctx, &config, &audio_state.device) != .SUCCESS {
		return false
	}
	if ma.device_start(&audio_state.device) != .SUCCESS {
		ma.device_uninit(&audio_state.device)
		return false
	}
	audio_state.dev_ok = true
	return true
}

// audio_shutdown stops the device, waits for the decode thread to exit
// and releases the audio stream. Call it once after audio_init.
audio_shutdown :: proc() {
	sync.atomic_store(&audio_state.stop, 1)
	if audio_state.dev_ok {
		ma.device_uninit(&audio_state.device)
		ma.context_uninit(&audio_state.ctx)
		audio_state.dev_ok = false
	}
	if audio_state.stream != nil {
		sync.sema_wait(&audio_state.exited)
		audio_stream_close(audio_state.stream)
		audio_state.stream = nil
	}
}

audio_set_volume :: proc(v: f32) {
	sync.atomic_store(&audio_state.volume, transmute(i64)u64(u32(transmute(u32)f32(v))))
}

audio_set_paused :: proc(p: bool) {
	v: i32 = 0
	if p {
		v = 1
	}
	sync.atomic_store(&audio_state.paused, v)
	if !p {
		sync.mutex_lock(&audio_state.mu)
		audio_state.anchor_consumed = audio_state.consumed
		audio_state.anchor_wall_ns = i64(time.since(audio_state.epoch))
		sync.mutex_unlock(&audio_state.mu)
	}
}

// audio_clock_sec returns the smoothed audio position in seconds. It
// interpolates linearly between sound card pulls so the paced clock has no
// staircase jitter, and freezes while paused.
audio_clock_sec :: proc() -> f64 {
	if sync.atomic_load(&audio_state.paused) != 0 {
		return f64(sync.atomic_load(&audio_state.anchor_consumed)) / f64(AUDIO_RATE)
	}
	sync.mutex_lock(&audio_state.mu)
	anchor := audio_state.anchor_consumed
	anchor_ns := audio_state.anchor_wall_ns
	sync.mutex_unlock(&audio_state.mu)
	elapsed := f64(i64(time.since(audio_state.epoch)) - anchor_ns) / 1e9
	return (f64(anchor) + elapsed * f64(AUDIO_RATE)) / f64(AUDIO_RATE)
}

AudioStream :: struct {
	fmt_ctx:    ^AVFormatContext,
	time_base:  f64,
	codec_ctx:  ^AVCodecContext,
	swr:        ^SwrContext,
	pkt:        ^AVPacket,
	frame:      ^AVFrame,
	stream_idx: int,
	samples:    []i16,
}

audio_stream_open :: proc(path: cstring) -> ^AudioStream {
	fmt_ctx: ^AVFormatContext
	if avformat_open_input(&fmt_ctx, path, nil, nil) != 0 {
		return nil
	}
	if avformat_find_stream_info(fmt_ctx, nil) < 0 {
		avformat_close_input(&fmt_ctx)
		return nil
	}
	idx := av_find_best_stream(fmt_ctx, AVMEDIA_TYPE_AUDIO, -1, -1, nil, 0)
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
	if avcodec_open2(codec_ctx, codec, nil) < 0 {
		avcodec_free_context(&codec_ctx)
		avformat_close_input(&fmt_ctx)
		return nil
	}

	out_layout: AVChannelLayout
	av_channel_layout_default(&out_layout, AUDIO_CHANNELS)
	swr: ^SwrContext
	in_fmt := c.int(par.format)
	if swr_alloc_set_opts2(&swr, &out_layout, AV_SAMPLE_FMT_S16, AUDIO_RATE,
		&par.ch_layout, in_fmt, par.sample_rate, 0, nil) < 0 {
		avcodec_free_context(&codec_ctx)
		avformat_close_input(&fmt_ctx)
		return nil
	}
	if swr_init(swr) < 0 {
		swr_free(&swr)
		avcodec_free_context(&codec_ctx)
		avformat_close_input(&fmt_ctx)
		return nil
	}

	s := new(AudioStream)
	s.fmt_ctx = fmt_ctx
	s.codec_ctx = codec_ctx
	s.swr = swr
	s.pkt = av_packet_alloc()
	s.frame = av_frame_alloc()
	s.stream_idx = int(idx)
	tb := s.fmt_ctx.streams[s.stream_idx].time_base
	s.time_base = f64(tb.num) / f64(tb.den)
	s.samples = make([]i16, AUDIO_RATE * AUDIO_CHANNELS)
	return s
}

audio_stream_close :: proc(s: ^AudioStream) {
	av_packet_free(&s.pkt)
	av_frame_free(&s.frame)
	swr_free(&s.swr)
	avcodec_free_context(&s.codec_ctx)
	avformat_close_input(&s.fmt_ctx)
	delete(s.samples)
	free(s)
}

// audio_seek requests a jump to the given media position. abs_ns is the
// matching absolute timeline position that the video clock follows.
audio_seek :: proc(media_ns: i64, abs_ns: i64) {
	sync.atomic_store(&audio_state.seek_req, media_ns)
	sync.atomic_store(&audio_state.seek_abs, abs_ns)
	sync.atomic_store(&audio_state.seek_flag, 1)
	sync.sema_post(&audio_state.space)
}

// audio_take_frame converts one decoded frame and feeds the ring,
// blocking while the ring is full.
audio_take_frame :: proc(s: ^AudioStream) {
	nb := int(s.frame.nb_samples)
	if nb <= 0 {
		av_frame_unref(s.frame)
		return
	}
	need := nb * AUDIO_CHANNELS
	if len(s.samples) < need {
		delete(s.samples)
		s.samples = make([]i16, need)
	}
	buf := s.samples[:need]
	out_planes: [1][^]u8
	out_planes[0] = ([^]u8)(&buf[0])
	in_count := swr_convert(s.swr, &out_planes[0], c.int(nb), &s.frame.extended_data[0], c.int(nb))
	if in_count > 0 {
		produced := int(in_count) * AUDIO_CHANNELS
		sync.mutex_lock(&audio_state.mu)
		written := ring_write(&audio_state.ring, buf[:produced])
		sync.mutex_unlock(&audio_state.mu)
		for written < produced {
			if sync.atomic_load(&audio_state.stop) != 0 {
				return
			}
			if !sync.sema_wait_with_timeout(&audio_state.space, 10 * time.Millisecond) {
				continue
			}
			if sync.atomic_load(&audio_state.seek_flag) != 0 {
				break
			}
			sync.mutex_lock(&audio_state.mu)
			written += ring_write(&audio_state.ring, buf[written:produced])
			sync.mutex_unlock(&audio_state.mu)
		}
	}
	av_frame_unref(s.frame)
}

audio_decode_thread :: proc(s: ^AudioStream) {
	defer sync.sema_post(&audio_state.exited)
	for {
		if sync.atomic_load(&audio_state.stop) != 0 {
			return
		}
		if sync.atomic_load(&audio_state.seek_flag) != 0 {
			media_ns := sync.atomic_load(&audio_state.seek_req)
			abs_ns := sync.atomic_load(&audio_state.seek_abs)
			sync.atomic_store(&audio_state.seek_flag, 0)
			ts := i64(f64(media_ns) / 1e9 / s.time_base)
			av_seek_frame(s.fmt_ctx, c.int(s.stream_idx), ts, AVSEEK_FLAG_BACKWARD)
			avcodec_flush_buffers(s.codec_ctx)
			cons := i64(f64(abs_ns) / 1e9 * f64(AUDIO_RATE))
			sync.mutex_lock(&audio_state.mu)
			audio_state.ring.head = audio_state.ring.tail
			audio_state.consumed = cons
			audio_state.anchor_consumed = cons
			audio_state.anchor_wall_ns = i64(time.since(audio_state.epoch))
			sync.mutex_unlock(&audio_state.mu)
			continue
		}
		res := av_read_frame(s.fmt_ctx, s.pkt)
		if res == AVERROR_EOF {
			av_seek_frame(s.fmt_ctx, c.int(s.stream_idx), 0, AVSEEK_FLAG_BACKWARD)
			continue
		}
		if res < 0 {
			return
		}
		if int(s.pkt.stream_index) != s.stream_idx {
			av_packet_unref(s.pkt)
			continue
		}
		for {
			sres := avcodec_send_packet(s.codec_ctx, s.pkt)
			if sres != AVERROR_EAGAIN {
				break
			}
			if avcodec_receive_frame(s.codec_ctx, s.frame) < 0 {
				break
			}
			audio_take_frame(s)
		}
		av_packet_unref(s.pkt)
		for {
			if avcodec_receive_frame(s.codec_ctx, s.frame) < 0 {
				break
			}
			audio_take_frame(s)
		}
	}
}

audio_start :: proc(path: cstring) -> bool {
	s := audio_stream_open(path)
	if s == nil {
		return false
	}
	audio_state.stream = s
	thread.create_and_start_with_data(s, proc(data: rawptr) {
		audio_decode_thread((^AudioStream)(data))
	})
	return true
}
