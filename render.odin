package aideo

import "core:os"
import "core:sync"
import "core:thread"
import "core:time"

// RenderJob is an immutable snapshot of everything the render thread
// needs to encode and write one frame.
RenderJob :: struct {
	buf:       []u8,
	pts_sec:   f64,
	w:         int,
	h:         int,
	start_col: int,
	start_row: int,
	pal:       ^SixelPalette,
	cache:     ^StripCache,
}

// RenderCtx decouples sixel encoding and terminal writes from the frame
// pacing thread. Only one job is queued at a time; a newer job replaces
// an older queued job so the render backlog stays bounded.
RenderCtx :: struct {
	mu:        sync.Mutex,
	job:       RenderJob,
	has:       bool,
	used:      sync.Sema,
	exited:    sync.Sema,
	stopped:   i32,
	vp:        ^VideoPlayer,
	pool:      ^StripPool,
	out:       Buffer,
	stdout_mu: sync.Mutex,
	written:   i64,
	freeze:    []u8,
	freeze_n:  int,
}

// render_submit hands a job to the render thread, replacing any queued job.
render_submit :: proc(rc: ^RenderCtx, job: RenderJob) {
	sync.mutex_lock(&rc.mu)
	if rc.has {
		video_pool_put(rc.vp, rc.job.buf)
	} else {
		sync.sema_post(&rc.used)
	}
	rc.job = job
	rc.has = true
	sync.mutex_unlock(&rc.mu)
}

// rerender_freeze re-renders the frozen frame with the current palette so
// palette changes are visible while playback is paused. The frozen frame
// is copied because the render thread owns the freeze buffer.
rerender_freeze :: proc(rc: ^RenderCtx, w, h, start_col, start_row: int, pal: ^SixelPalette, cache: ^StripCache) {
	sync.mutex_lock(&rc.mu)
	fb := rc.freeze
	fn := rc.freeze_n
	sync.mutex_unlock(&rc.mu)
	if fn != w * h * 4 || len(fb) < fn {
		return
	}
	cp := video_pool_get(rc.vp, fn)
	copy(cp, fb[:fn])
	render_submit(rc, RenderJob{
		buf = cp,
		w = w,
		h = h,
		start_col = start_col,
		start_row = start_row,
		pal = pal,
		cache = cache,
	})
}

// render_thread encodes queued frames and writes them to the terminal.
render_thread :: proc(data: rawptr) {
	rc := (^RenderCtx)(data)
	defer sync.sema_post(&rc.exited)
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
		if len(job.buf) == 0 {
			continue
		}
		buffer_reset(&rc.out)
		write_bytes(&rc.out, []u8{0x1b})
		write_byte(&rc.out, '[')
		write_sixel_num(&rc.out, job.start_row)
		write_byte(&rc.out, ';')
		write_sixel_num(&rc.out, job.start_col)
		write_byte(&rc.out, 'H')
		sixel_encode(rc.pool, &rc.out, job.buf, job.w, job.h, job.pal, job.cache)
		_t4 := time.now()
		sync.mutex_lock(&rc.stdout_mu)
		wdata := rc.out.buf[:rc.out.pos]
		for len(wdata) > 0 {
			nw, werr := os.write(os.stdout, wdata)
			if werr != nil || nw <= 0 {
				break
			}
			wdata = wdata[nw:]
		}
		sync.mutex_unlock(&rc.stdout_mu)
		prof_add(PROF_WRITE, i64(time.since(_t4)))
		sync.atomic_add(&rc.written, 1)
		sync.mutex_lock(&rc.mu)
		old := rc.freeze
		rc.freeze = job.buf
		rc.freeze_n = len(job.buf)
		sync.mutex_unlock(&rc.mu)
		if len(old) > 0 {
			video_pool_put(rc.vp, old)
		}
	}
}

// render_stop wakes the render thread, drops any queued job and waits
// for the render thread to exit.
render_stop :: proc(rc: ^RenderCtx) {
	sync.atomic_store(&rc.stopped, 1)
	sync.sema_post(&rc.used)
	sync.sema_wait(&rc.exited)
}
