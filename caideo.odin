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
	border_px:  c.int,
	border_color: c.int,
}

// Engine is the C ABI handle for one rendering instance. It owns the
// render pipeline, the strip workers and the yuv scaler.
Engine :: struct {
	mu:       sync.Mutex,
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
	border_px: int,
	border_color: u32,
	need_full: i32,
}

// frame_border paints a solid frame around one RGBA buffer in place
// so the picture area is always visible against the terminal. The
// color is 0xRRGGBB.
frame_border :: proc(p: []u8, w, h, px: int, color: u32) {
	if px <= 0 || w <= px * 2 || h <= px * 2 {
		return
	}
	cr := u8(color >> 16)
	cg := u8(color >> 8)
	cb := u8(color)
	for y in 0..<px {
		for x in 0..<w {
			fill_px(p, w, y, x, cr, cg, cb)
		}
		for x in 0..<w {
			fill_px(p, w, h - 1 - y, x, cr, cg, cb)
		}
	}
	for y in px..<(h - px) {
		for x in 0..<px {
			fill_px(p, w, y, x, cr, cg, cb)
		}
		for x in (w - px)..<w {
			fill_px(p, w, y, x, cr, cg, cb)
		}
	}
}

// fill_px writes one border pixel into an RGBA buffer.
fill_px :: proc(p: []u8, w, y, x: int, cr, cg, cb: u8) {
	o := (y * w + x) * 4
	p[o] = cr
	p[o + 1] = cg
	p[o + 2] = cb
	p[o + 3] = 0xff
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
	reset := sync.atomic_exchange(&e.need_full, 0) != 0
	if sync.atomic_exchange(&e.rc.lost, 0) != 0 {
		reset = true
	}
	render_submit(&e.rc, RenderJob{
		buf = buf,
		w = e.out_w,
		h = e.out_h,
		start_col = e.col,
		start_row = e.row,
		pal = e.pal,
		reset_cache = reset,
	})
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
	e.border_px = max(int(cfg.border_px), 0)
	e.border_color = u32(cfg.border_color)
	if e.border_color == 0 {
		e.border_color = 0xd8d8d8
	}
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
	sync.mutex_lock(&e.mu)
	render_stop(&e.rc)
	sync.mutex_unlock(&e.mu)
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
	sync.mutex_lock(&stdout_mu)
	varr: IoVecList
	iovs_push(&varr, CLEAR_BYTES[:])
	_ = writev(c.int(e.rc.fd), &varr.iovs[0], 1)
	sync.mutex_unlock(&stdout_mu)
	free(e.vp)
	free(e)
}

// aideo_resize changes the drawn size and origin without touching the
// screen, so a caller that relayouts several engines wipes the old
// picture area once instead of once per engine. The next frame is
// drawn in full at the new geometry.
@(export)
aideo_resize :: proc "c" (e: ^Engine, out_w, out_h, origin_row, origin_col: c.int) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	sync.mutex_lock(&e.mu)
	defer sync.mutex_unlock(&e.mu)
	e.out_w = max(int(out_w), 4)
	e.out_h = max(int(out_h), 4)
	e.row = max(int(origin_row), 1)
	e.col = max(int(origin_col), 1)
	engine_build_plan(e)
	sync.atomic_store(&e.need_full, 1)
	if len(e.rc.freeze) > 0 {
		rerender_freeze_scaled(&e.rc, e.out_w, e.out_h, e.col, e.row, e.pal)
	}
}

@(export)
aideo_set_colors :: proc "c" (e: ^Engine, colors: c.int) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	sync.mutex_lock(&e.mu)
	defer sync.mutex_unlock(&e.mu)
	e.pal = get_palette(engine_default_colors(int(colors)))
	sync.atomic_store(&e.need_full, 1)
}

@(export)
aideo_push_rgba :: proc "c" (e: ^Engine, rgba: [^]u8, stride: c.int) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	sync.mutex_lock(&e.mu)
	defer sync.mutex_unlock(&e.mu)
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
	if e.border_px > 0 {
		frame_border(buf[:e.out_w * e.out_h * 4], e.out_w, e.out_h, e.border_px, e.border_color)
	}
	engine_submit(e, buf)
}

@(export)
aideo_push_yuv :: proc "c" (e: ^Engine, y, u, v: [^]u8, stride_y, stride_c: c.int) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	sync.mutex_lock(&e.mu)
	defer sync.mutex_unlock(&e.mu)
	buf := video_pool_get(e.vp, e.out_w * e.out_h * 4)
	scale_frame(&e.spool, e.plan, y, u, v, int(stride_y), int(stride_c), &buf[0], e.out_w * 4)
	if e.border_px > 0 {
		frame_border(buf[:e.out_w * e.out_h * 4], e.out_w, e.out_h, e.border_px, e.border_color)
	}
	engine_submit(e, buf)
}

@(export)
aideo_clear :: proc "c" (e: ^Engine) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	sync.mutex_lock(&e.mu)
	defer sync.mutex_unlock(&e.mu)
	sync.mutex_lock(&e.rc.mu)
	if len(e.rc.freeze) > 0 {
		video_pool_put(e.vp, e.rc.freeze)
		e.rc.freeze = nil
		e.rc.freeze_n = 0
	}
	sync.mutex_unlock(&e.rc.mu)
	render_submit(&e.rc, RenderJob{clear = true})
	sync.atomic_store(&e.need_full, 1)
}

@(export)
aideo_repaint :: proc "c" (e: ^Engine) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	sync.mutex_lock(&e.mu)
	defer sync.mutex_unlock(&e.mu)
	sync.atomic_store(&e.need_full, 1)
}

// aideo_restore redraws the frozen frame at once with a full encode
// so a picture survives a terminal clear without waiting for a new
// frame.
@(export)
aideo_restore :: proc "c" (e: ^Engine) {
	context = runtime.default_context()
	if e == nil {
		return
	}
	sync.mutex_lock(&e.mu)
	defer sync.mutex_unlock(&e.mu)
	sync.atomic_store(&e.need_full, 1)
	rerender_freeze(&e.rc, e.out_w, e.out_h, e.col, e.row, e.pal)
}

// aideo_stdout_lock serializes one caller's terminal writes against
// every render thread so text and sixel streams never interleave.
@(export)
aideo_stdout_lock :: proc "c" () {
	sync.mutex_lock(&stdout_mu)
}

// aideo_stdout_unlock releases the terminal write lock.
@(export)
aideo_stdout_unlock :: proc "c" () {
	sync.mutex_unlock(&stdout_mu)
}
