package aideo

import "core:c"
import "core:sync"
import "core:thread"
import "core:time"

CLEAR_BYTES := [11]u8{0x1b, '[', '2', 'J', 0x1b, '[', '3', 'J', 0x1b, '[', 'H'}

// RenderJob is an immutable snapshot of everything the render pipeline
// needs to encode and write one frame. A clear job only wipes the screen.
RenderJob :: struct {
	buf:       []u8,
	w:         int,
	h:         int,
	start_col: int,
	start_row: int,
	pal:       ^SixelPalette,
	reset_cache: bool,
	keep_freeze: bool,
	clear:     bool,
}

// stdout_mu serializes terminal writes across every engine so two
// sixel streams never interleave and tear each other apart.
stdout_mu: sync.Mutex

// RenderCtx runs encoding and terminal writes on separate threads so the
// next frame is encoded while the previous one is still being written.
// Only jobs that have not been encoded may be dropped, so the diff cache
// always matches what the terminal displays.
RenderCtx :: struct {
	mu:        sync.Mutex,
	job:       RenderJob,
	has:       bool,
	used:      sync.Sema,

	wmu:       sync.Mutex,
	wjob:      RenderJob,
	wdata:     []u8,
	wbuf:      int,
	wclear:    bool,
	wused:     sync.Sema,
	wempty:    sync.Sema,

	buf_mu:    sync.Mutex,
	bufs:      [3]Buffer,
	buf_taken: [3]bool,
	buf_empty: sync.Sema,

	exited:    sync.Sema,
	stopped:   i32,

	vp:        ^VideoPlayer,
	pool:      ^StripPool,
	fd:        int,
	written:   i64,
	freeze:    []u8,
	freeze_n:  int,
	freeze_w:  int,
	freeze_h:  int,

	cache:     ^StripCache,
	cache_w:   int,
	cache_h:   int,
}

// render_submit hands a job to the encode thread, replacing any queued
// job. A queued clear request is inherited by its replacement so a screen
// wipe can never be lost behind a later frame.
render_submit :: proc(rc: ^RenderCtx, job: RenderJob) {
	j := job
	sync.mutex_lock(&rc.mu)
	if rc.has {
		if len(rc.job.buf) > 0 {
			video_pool_put(rc.vp, rc.job.buf)
		}
		if rc.job.clear && !j.clear {
			j.clear = true
		}
		if rc.job.reset_cache && !j.reset_cache {
			j.reset_cache = true
		}
	} else {
		sync.sema_post(&rc.used)
	}
	rc.job = j
	rc.has = true
	sync.mutex_unlock(&rc.mu)
}

// rerender_freeze re-renders the frozen frame with the current palette so
// palette changes are visible while playback is paused. The frozen frame
// is copied under rc.mu so the write thread can never reclaim it mid copy.
rerender_freeze :: proc(rc: ^RenderCtx, w, h, start_col, start_row: int, pal: ^SixelPalette) {
	cp: []u8
	sync.mutex_lock(&rc.mu)
	fb := rc.freeze
	fn := rc.freeze_n
	if fn == w * h * 4 && len(fb) >= fn {
		cp = video_pool_get(rc.vp, fn)
		copy(cp, fb[:fn])
	}
	sync.mutex_unlock(&rc.mu)
	if len(cp) == 0 {
		return
	}
	render_submit(rc, RenderJob{
		buf = cp,
		w = w,
		h = h,
		start_col = start_col,
		start_row = start_row,
		pal = pal,
		reset_cache = true,
	})
}

// rerender_freeze_scaled redraws the frozen frame at a new size with
// nearest neighbor sampling so a paused picture keeps its content when
// the display size changes.
rerender_freeze_scaled :: proc(rc: ^RenderCtx, w, h, start_col, start_row: int, pal: ^SixelPalette) {
	sw := 0
	sh := 0
	cp: []u8
	sync.mutex_lock(&rc.mu)
	fn := rc.freeze_n
	if fn > 0 && len(rc.freeze) >= fn && rc.freeze_w > 0 && rc.freeze_h > 0 {
		sw = rc.freeze_w
		sh = rc.freeze_h
		if sw * sh * 4 == fn {
			cp = video_pool_get(rc.vp, fn)
			copy(cp, rc.freeze[:fn])
		}
	}
	sync.mutex_unlock(&rc.mu)
	if len(cp) == 0 {
		return
	}
	dst := video_pool_get(rc.vp, w * h * 4)
	for dy in 0..<h {
		sy := dy * sh / h
		srow := sy * sw * 4
		drow := dy * w * 4
		for dx in 0..<w {
			sx := dx * sw / w
			copy(dst[drow + dx * 4:], cp[srow + sx * 4:srow + sx * 4 + 4])
		}
	}
	video_pool_put(rc.vp, cp)
	render_submit(rc, RenderJob{
		buf = dst,
		w = w,
		h = h,
		start_col = start_col,
		start_row = start_row,
		pal = pal,
		reset_cache = true,
		keep_freeze = true,
	})
}

// render_buf_get acquires a free assembly buffer.
render_buf_get :: proc(rc: ^RenderCtx) -> int {
	sync.sema_wait(&rc.buf_empty)
	sync.mutex_lock(&rc.buf_mu)
	idx := -1
	for i in 0..<len(rc.bufs) {
		if !rc.buf_taken[i] {
			rc.buf_taken[i] = true
			idx = i
			break
		}
	}
	sync.mutex_unlock(&rc.buf_mu)
	return idx
}

// render_buf_put returns an assembly buffer to the free pool.
render_buf_put :: proc(rc: ^RenderCtx, idx: int) {
	sync.mutex_lock(&rc.buf_mu)
	rc.buf_taken[idx] = false
	sync.mutex_unlock(&rc.buf_mu)
	sync.sema_post(&rc.buf_empty)
}

// render_handoff passes one assembled frame to the write thread, blocking
// while the write slot is occupied.
render_handoff :: proc(rc: ^RenderCtx, job: RenderJob, data: []u8, buf_idx: int, clear: bool) {
	sync.sema_wait(&rc.wempty)
	sync.mutex_lock(&rc.wmu)
	rc.wjob = job
	rc.wdata = data
	rc.wbuf = buf_idx
	rc.wclear = clear
	sync.mutex_unlock(&rc.wmu)
	sync.sema_post(&rc.wused)
}

// render_encode_thread assembles frames into self contained buffers. It
// owns the strip diff cache: the cache is rebuilt whenever the frame
// geometry or the palette changes so no other thread ever touches it.
render_encode_thread :: proc(data: rawptr) {
	rc := (^RenderCtx)(data)
	defer sync.sema_post(&rc.exited)
	defer if rc.cache != nil {
		strip_cache_free(rc.cache)
		rc.cache = nil
	}
	for {
		sync.sema_wait(&rc.used)
		sync.mutex_lock(&rc.mu)
		job := rc.job
		rc.job = {}
		rc.has = false
		stopped := sync.atomic_load(&rc.stopped) != 0
		sync.mutex_unlock(&rc.mu)
		if stopped {
			if len(job.buf) > 0 {
				video_pool_put(rc.vp, job.buf)
			}
			return
		}
		if job.clear && rc.cache != nil {
			strip_cache_free(rc.cache)
			rc.cache = nil
		}
		if len(job.buf) == 0 {
			if job.clear {
				render_handoff(rc, job, nil, -1, true)
			}
			continue
		}
		idx := render_buf_get(rc)
		buf := &rc.bufs[idx]
		buffer_reset(buf)
		write_bytes(buf, []u8{0x1b})
		write_byte(buf, '[')
		write_sixel_num(buf, job.start_row)
		write_byte(buf, ';')
		write_sixel_num(buf, job.start_col)
		write_byte(buf, 'H')
		if rc.cache == nil || rc.cache_w != job.w || rc.cache_h != job.h || job.reset_cache {
			if rc.cache != nil {
				strip_cache_free(rc.cache)
			}
			rc.cache = new(StripCache)
			strip_cache_init(rc.cache, (job.h + 5) / 6)
			rc.cache_w = job.w
			rc.cache_h = job.h
		}
		sixel_encode(rc.pool, buf, job.buf, job.w, job.h, job.pal, rc.cache)
		write_bytes(buf, []u8{0x1b})
		write_byte(buf, '[')
		write_sixel_num(buf, job.start_row)
		write_byte(buf, ';')
		write_sixel_num(buf, job.start_col)
		write_byte(buf, 'H')
		render_handoff(rc, job, buf.buf[:buf.pos], idx, job.clear)
	}
}

// render_write_thread writes assembled frames to the terminal in order.
render_write_thread :: proc(data: rawptr) {
	rc := (^RenderCtx)(data)
	defer sync.sema_post(&rc.exited)
	for {
		sync.sema_wait(&rc.wused)
		sync.mutex_lock(&rc.wmu)
		job := rc.wjob
		wdata := rc.wdata
		wbuf := rc.wbuf
		wclear := rc.wclear
		rc.wjob = {}
		rc.wdata = nil
		rc.wclear = false
		sync.mutex_unlock(&rc.wmu)
		sync.sema_post(&rc.wempty)
		stopped := sync.atomic_load(&rc.stopped) != 0
		if wclear {
			varr: IoVecList
			iovs_push(&varr, CLEAR_BYTES[:])
			sync.mutex_lock(&stdout_mu)
			writev_all(rc.fd, &varr)
			sync.mutex_unlock(&stdout_mu)
		}
		if len(wdata) == 0 {
			if stopped {
				return
			}
			continue
		}
		_t4 := time.now()
		varr: IoVecList
		iovs_push(&varr, wdata)
		sync.mutex_lock(&stdout_mu)
		writev_all(rc.fd, &varr)
		sync.mutex_unlock(&stdout_mu)
		prof_add(PROF_WRITE, i64(time.since(_t4)))
		sync.atomic_add(&rc.written, 1)
		sync.mutex_lock(&rc.mu)
		old: []u8
		if job.keep_freeze {
			old = job.buf
		} else {
			old = rc.freeze
			rc.freeze = job.buf
			rc.freeze_n = len(job.buf)
			rc.freeze_w = job.w
			rc.freeze_h = job.h
		}
		sync.mutex_unlock(&rc.mu)
		if len(old) > 0 {
			video_pool_put(rc.vp, old)
		}
		render_buf_put(rc, wbuf)
		if stopped {
			return
		}
	}
}

// writev_all writes the whole iovec list to fd, handling partial writes.
writev_all :: proc(fd: int, l: ^IoVecList) {
	base := 0
	for base < l.n {
		cnt := min(l.n - base, 1024)
		nw := writev(c.int(fd), &l.iovs[base], c.int(cnt))
		if nw <= 0 {
			return
		}
		remain := int(nw)
		for remain > 0 && base < l.n {
			sz := int(l.iovs[base].iov_len)
			if sz <= remain {
				remain -= sz
				base += 1
			} else {
				p := ([^]u8)(l.iovs[base].iov_base)
				l.iovs[base].iov_base = &p[remain]
				l.iovs[base].iov_len = u64(sz - remain)
				remain = 0
			}
		}
	}
}

// render_start spawns the encode and write threads.
render_start :: proc(rc: ^RenderCtx) {
	if rc.fd == 0 {
		rc.fd = 1
	}
	sync.sema_post(&rc.buf_empty, len(rc.bufs))
	sync.sema_post(&rc.wempty, 1)
	thread.create_and_start_with_data(rc, render_encode_thread, nil, .Normal, true)
	thread.create_and_start_with_data(rc, render_write_thread, nil, .Normal, true)
}

// render_stop wakes both threads, drops queued work and waits for exit.
render_stop :: proc(rc: ^RenderCtx) {
	sync.atomic_store(&rc.stopped, 1)
	sync.sema_post(&rc.used)
	sync.sema_post(&rc.wused)
	sync.sema_wait(&rc.exited)
	sync.sema_wait(&rc.exited)
}
