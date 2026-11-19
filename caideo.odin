package aideo

import "core:c"
import "base:runtime"
import "core:sync"

// aideo_config mirrors the C struct of the same name in aideo.h.
aideo_config :: struct {
	fd:         c.int,
	src_w:      c.int,
	src_h:      c.int,
	out_w:      c.int,
	out_h:      c.int,
	colors:     c.int,
	origin_row: c.int,
	origin_col: c.int,
}

// Engine is the C ABI handle for one rendering instance. It owns the
// render pipeline, the strip workers and the yuv scaler.
Engine :: struct {
	rc:       RenderCtx,
	pool:     StripPool,
	spool:    ScalePool,
	vp:       ^VideoPlayer,
	pal:      ^SixelPalette,
	plan:     ^ScalePlan,
	src_w:    int,
	src_h:    int,
	out_w:    int,
	out_h:    int,
	row:      int,
	col:      int,
	need_full: bool,
}

// engine_default_colors clamps an unsupported color count to 64.
engine_default_colors :: proc(colors: int) -> int {
	for lv in PALETTE_LEVELS {
		if lv == colors {
			return colors
		}
	}
	return 64
}

engine_build_plan :: proc(e: ^Engine) {
	if e.plan != nil {
		scale_plan_free(e.plan)
	}
	e.plan = scale_plan(e.src_w, e.src_h, e.out_w, e.out_h)
}

engine_submit :: proc(e: ^Engine, buf: []u8) {
	render_submit(&e.rc, RenderJob{
		buf = buf,
		w = e.out_w,
		h = e.out_h,
		start_col = e.col,
		start_row = e.row,
		pal = e.pal,
		reset_cache = e.need_full,
	})
	e.need_full = false
}

@(export)
aideo_open :: proc "c" (cfg: ^aideo_config) -> ^Engine {
	context = runtime.default_context()
	if cfg == nil {
		return nil
	}
	e := new(Engine)
	e.src_w = max(int(cfg.src_w), 1)
	e.src_h = max(int(cfg.src_h), 1)
	e.out_w = max(int(cfg.out_w), 4)
	e.out_h = max(int(cfg.out_h), 4)
	e.row = max(int(cfg.origin_row), 1)
	e.col = max(int(cfg.origin_col), 1)
	e.pal = get_palette(engine_default_colors(int(cfg.colors)))
	e.vp = new(VideoPlayer)
	e.rc.vp = e.vp
	e.rc.fd = int(cfg.fd)
	if e.rc.fd <= 0 {
		e.rc.fd = 1
	}
	strip_pool_init(&e.pool, max(cpu_count() / 2, 4))
	e.rc.pool = &e.pool
	for i in 0..<len(e.rc.bufs) {
		buffer_init(&e.rc.bufs[i], e.out_w * e.out_h / 2)
	}
	scale_pool_init(&e.spool, max(cpu_count() / 2, 2))
	engine_build_plan(e)
	render_start(&e.rc)
	return e
}

@(export)
aideo_close :: proc "c" (e: ^Engine) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	render_stop(&e.rc)
	if len(e.rc.freeze) > 0 {
		video_pool_put(e.vp, e.rc.freeze)
		e.rc.freeze = nil
	}
	scale_pool_stop(&e.spool)
	strip_pool_stop(&e.pool)
	if e.plan != nil {
		scale_plan_free(e.plan)
	}
	sync.mutex_lock(&e.vp.pool_mu)
	for i in 0..<len(e.vp.pool) {
		if e.vp.pool[i] != nil {
			free(&e.vp.pool[i][0])
			e.vp.pool[i] = nil
		}
	}
	sync.mutex_unlock(&e.vp.pool_mu)
	for i in 0..<len(e.rc.bufs) {
		if len(e.rc.bufs[i].buf) > 0 {
			free(&e.rc.bufs[i].buf[0])
		}
	}
	free(e.vp)
	free(e)
}

@(export)
aideo_resize :: proc "c" (e: ^Engine, out_w, out_h, origin_row, origin_col: c.int) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	e.out_w = max(int(out_w), 4)
	e.out_h = max(int(out_h), 4)
	e.row = max(int(origin_row), 1)
	e.col = max(int(origin_col), 1)
	engine_build_plan(e)
	render_submit(&e.rc, RenderJob{clear = true})
	e.need_full = true
}

@(export)
aideo_set_colors :: proc "c" (e: ^Engine, colors: c.int) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	e.pal = get_palette(engine_default_colors(int(colors)))
	e.need_full = true
}

@(export)
aideo_push_rgba :: proc "c" (e: ^Engine, rgba: [^]u8, stride: c.int) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	buf := video_pool_get(e.vp, e.out_w * e.out_h * 4)
	st := int(stride)
	if st == e.out_w * 4 {
		copy(buf, rgba[:e.out_w * e.out_h * 4])
	} else {
		for y in 0..<e.out_h {
			off := y * st
			copy(buf[y * e.out_w * 4:], rgba[off:off + e.out_w * 4])
		}
	}
	engine_submit(e, buf)
}

@(export)
aideo_push_yuv :: proc "c" (e: ^Engine, y, u, v: [^]u8, stride_y, stride_c: c.int) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	buf := video_pool_get(e.vp, e.out_w * e.out_h * 4)
	scale_frame(&e.spool, e.plan, y, u, v, int(stride_y), int(stride_c), &buf[0], e.out_w * 4)
	engine_submit(e, buf)
}

@(export)
aideo_clear :: proc "c" (e: ^Engine) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	render_submit(&e.rc, RenderJob{clear = true})
	e.need_full = true
}

@(export)
aideo_repaint :: proc "c" (e: ^Engine) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	e.need_full = true
}
