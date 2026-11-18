package aideo

import "core:c"
import "core:sync"
import "core:thread"

// ScalePlan holds precomputed box filter weights for one output geometry.
// Every output row is computed independently from its source rows, so the
// scaling is exact under any thread split and has no seam artifacts.
ScalePlan :: struct {
	sw, sh:  int,
	sw2, sh2: int,
	dw, dh:  int,
	vy_off:  []i32,
	vy_cnt:  []i32,
	vy_woff: []i32,
	vy_w:    []u32,
	vcy_off: []i32,
	vcy_cnt: []i32,
	vcy_woff: []i32,
	vcy_w:   []u32,
	hx_off:  []i32,
	hx_cnt:  []i32,
	hx_woff: []i32,
	hx_w:    []u32,
	hcx_off: []i32,
	hcx_cnt: []i32,
	hcx_woff: []i32,
	hcx_w:   []u32,
	inv_sh:  u64,
	inv_sh2: u64,
	inv_sw:  u64,
	inv_sw2: u64,
}

// scale_axis builds one axis of box weights. Output index i covers the
// source interval i*src_len/out_len and the weights sum to src_len.
scale_axis :: proc(out_len, src_len: int, off: []i32, cnt: []i32, woff: []i32, w: []u32) {
	wi := 0
	for i in 0..<out_len {
		span0 := i * src_len
		span1 := (i + 1) * src_len
		first := span0 / out_len
		last := (span1 - 1) / out_len
		off[i] = i32(first)
		cnt[i] = i32(last - first + 1)
		woff[i] = i32(wi)
		for r in first..=last {
			a := max(span0, r * out_len)
			b := min(span1, (r + 1) * out_len)
			w[wi] = u32(b - a)
			wi += 1
		}
	}
}

// scale_inv computes a reciprocal for division-free normalization.
scale_inv :: proc(src_len: int) -> u64 {
	return (u64(1) << 32) / u64(src_len)
}

// scale_plan builds the weight tables for scaling sw by sh to dw by dh.
scale_plan :: proc(sw, sh, dw, dh: int) -> ^ScalePlan {
	p := new(ScalePlan)
	p.sw, p.sh = sw, sh
	p.sw2, p.sh2 = (sw + 1) / 2, (sh + 1) / 2
	p.dw, p.dh = dw, dh
	p.vy_off = make([]i32, dh)
	p.vy_cnt = make([]i32, dh)
	p.vy_woff = make([]i32, dh)
	p.vy_w = make([]u32, sh + dh + 4)
	scale_axis(dh, sh, p.vy_off, p.vy_cnt, p.vy_woff, p.vy_w)
	p.vcy_off = make([]i32, dh)
	p.vcy_cnt = make([]i32, dh)
	p.vcy_woff = make([]i32, dh)
	p.vcy_w = make([]u32, p.sh2 + dh + 4)
	scale_axis(dh, p.sh2, p.vcy_off, p.vcy_cnt, p.vcy_woff, p.vcy_w)
	p.hx_off = make([]i32, dw)
	p.hx_cnt = make([]i32, dw)
	p.hx_woff = make([]i32, dw)
	p.hx_w = make([]u32, sw + dw + 4)
	scale_axis(dw, sw, p.hx_off, p.hx_cnt, p.hx_woff, p.hx_w)
	p.hcx_off = make([]i32, dw)
	p.hcx_cnt = make([]i32, dw)
	p.hcx_woff = make([]i32, dw)
	p.hcx_w = make([]u32, p.sw2 + dw + 4)
	scale_axis(dw, p.sw2, p.hcx_off, p.hcx_cnt, p.hcx_woff, p.hcx_w)
	p.inv_sh = scale_inv(sh)
	p.inv_sh2 = scale_inv(p.sh2)
	p.inv_sw = scale_inv(sw)
	p.inv_sw2 = scale_inv(p.sw2)
	return p
}

// ScaleJob describes one frame being scaled.
ScaleJob :: struct {
	plan:      ^ScalePlan,
	src_y:     [^]u8,
	src_u:     [^]u8,
	src_v:     [^]u8,
	stride_y:  int,
	stride_c:  int,
	dst:       [^]u8,
	dst_stride: int,
	next_dy:   i64,
}

// ScaleWorkerState is the per worker scratch for scaling.
ScaleWorkerState :: struct {
	tmp_y: []u8,
	tmp_u: []u8,
	tmp_v: []u8,
	ty:    []u8,
	tu:    []u8,
	tv:    []u8,
	acc_y: []u32,
	acc_c: []u32,
}

// scale_prepare_scratch sizes the per worker scratch rows.
scale_prepare_scratch :: proc(ws: ^ScaleWorkerState, p: ^ScalePlan) {
	if len(ws.tmp_y) < p.sw {
		ws.tmp_y = make([]u8, p.sw)
		ws.acc_y = make([]u32, p.sw)
	}
	if len(ws.tmp_u) < p.sw2 {
		ws.tmp_u = make([]u8, p.sw2)
		ws.tmp_v = make([]u8, p.sw2)
		ws.acc_c = make([]u32, p.sw2)
	}
	if len(ws.ty) < p.dw {
		ws.ty = make([]u8, p.dw)
		ws.tu = make([]u8, p.dw)
		ws.tv = make([]u8, p.dw)
	}
}

// scale_row scales one output row with the BT.601 limited range matrix.
scale_row :: proc(ws: ^ScaleWorkerState, job: ^ScaleJob, dy: int) {
	p := job.plan
	y0 := int(p.vy_off[dy])
	yn := int(p.vy_cnt[dy])
	yw := int(p.vy_woff[dy])
	cy0 := int(p.vcy_off[dy])
	cyn := int(p.vcy_cnt[dy])
	cyw := int(p.vcy_woff[dy])

	vert_acc_init_avx2(&ws.acc_y[0], ([^]u8)(&job.src_y[y0 * job.stride_y]), u32(p.vy_w[yw]), c.int(p.sw))
	for k in 1..<yn {
		vert_acc_avx2(&ws.acc_y[0], ([^]u8)(&job.src_y[(y0 + k) * job.stride_y]), u32(p.vy_w[yw + k]), c.int(p.sw))
	}
	norm_acc_avx2(&ws.tmp_y[0], &ws.acc_y[0], u32(p.inv_sh), c.int(p.sw))

	vert_acc_init_avx2(&ws.acc_c[0], ([^]u8)(&job.src_u[cy0 * job.stride_c]), u32(p.vcy_w[cyw]), c.int(p.sw2))
	for k in 1..<cyn {
		vert_acc_avx2(&ws.acc_c[0], ([^]u8)(&job.src_u[(cy0 + k) * job.stride_c]), u32(p.vcy_w[cyw + k]), c.int(p.sw2))
	}
	norm_acc_avx2(&ws.tmp_u[0], &ws.acc_c[0], u32(p.inv_sh2), c.int(p.sw2))

	vert_acc_init_avx2(&ws.acc_c[0], ([^]u8)(&job.src_v[cy0 * job.stride_c]), u32(p.vcy_w[cyw]), c.int(p.sw2))
	for k in 1..<cyn {
		vert_acc_avx2(&ws.acc_c[0], ([^]u8)(&job.src_v[(cy0 + k) * job.stride_c]), u32(p.vcy_w[cyw + k]), c.int(p.sw2))
	}
	norm_acc_avx2(&ws.tmp_v[0], &ws.acc_c[0], u32(p.inv_sh2), c.int(p.sw2))

	for dx in 0..<p.dw {
		x0 := int(p.hx_off[dx])
		xn := int(p.hx_cnt[dx])
		xw := int(p.hx_woff[dx])
		acc: u32
		for k in 0..<xn {
			acc += u32(p.hx_w[xw + k]) * u32(ws.tmp_y[x0 + k])
		}
		ws.ty[dx] = u8((u64(acc) * p.inv_sw + (u64(1) << 31)) >> 32)

		cx0 := int(p.hcx_off[dx])
		cxn := int(p.hcx_cnt[dx])
		cxw := int(p.hcx_woff[dx])
		acc = 0
		for k in 0..<cxn {
			acc += u32(p.hcx_w[cxw + k]) * u32(ws.tmp_u[cx0 + k])
		}
		ws.tu[dx] = u8((u64(acc) * p.inv_sw2 + (u64(1) << 31)) >> 32)
		acc = 0
		for k in 0..<cxn {
			acc += u32(p.hcx_w[cxw + k]) * u32(ws.tmp_v[cx0 + k])
		}
		ws.tv[dx] = u8((u64(acc) * p.inv_sw2 + (u64(1) << 31)) >> 32)
	}

	out := ([^]u8)(&job.dst[dy * job.dst_stride])
	rgba_matrix_avx2(&out[0], &ws.ty[0], &ws.tu[0], &ws.tv[0], c.int(p.dw))
}

// scale_row_ref is the scalar reference implementation kept for parity
// testing. The fast path must stay byte identical to it.
scale_row_ref :: proc(ws: ^ScaleWorkerState, job: ^ScaleJob, dy: int) {
	p := job.plan
	y0 := int(p.vy_off[dy])
	yn := int(p.vy_cnt[dy])
	yw := int(p.vy_woff[dy])
	cy0 := int(p.vcy_off[dy])
	cyn := int(p.vcy_cnt[dy])
	cyw := int(p.vcy_woff[dy])

	for i in 0..<p.sw {
		ws.acc_y[i] = 0
	}
	for k in 0..<yn {
		w := u32(p.vy_w[yw + k])
		row := ([^]u8)(&job.src_y[(y0 + k) * job.stride_y])
		for i in 0..<p.sw {
			ws.acc_y[i] += w * u32(row[i])
		}
	}
	for i in 0..<p.sw {
		ws.tmp_y[i] = u8((u64(ws.acc_y[i]) * p.inv_sh + (u64(1) << 31)) >> 32)
	}

	for i in 0..<p.sw2 {
		ws.acc_c[i] = 0
	}
	for k in 0..<cyn {
		w := u32(p.vcy_w[cyw + k])
		row := ([^]u8)(&job.src_u[(cy0 + k) * job.stride_c])
		for i in 0..<p.sw2 {
			ws.acc_c[i] += w * u32(row[i])
		}
	}
	for i in 0..<p.sw2 {
		ws.tmp_u[i] = u8((u64(ws.acc_c[i]) * p.inv_sh2 + (u64(1) << 31)) >> 32)
	}

	for i in 0..<p.sw2 {
		ws.acc_c[i] = 0
	}
	for k in 0..<cyn {
		w := u32(p.vcy_w[cyw + k])
		row := ([^]u8)(&job.src_v[(cy0 + k) * job.stride_c])
		for i in 0..<p.sw2 {
			ws.acc_c[i] += w * u32(row[i])
		}
	}
	for i in 0..<p.sw2 {
		ws.tmp_v[i] = u8((u64(ws.acc_c[i]) * p.inv_sh2 + (u64(1) << 31)) >> 32)
	}

	out := ([^]u8)(&job.dst[dy * job.dst_stride])
	for dx in 0..<p.dw {
		x0 := int(p.hx_off[dx])
		xn := int(p.hx_cnt[dx])
		xw := int(p.hx_woff[dx])
		acc: u32
		for k in 0..<xn {
			acc += u32(p.hx_w[xw + k]) * u32(ws.tmp_y[x0 + k])
		}
		yv := i32((u64(acc) * p.inv_sw + (u64(1) << 31)) >> 32)

		cx0 := int(p.hcx_off[dx])
		cxn := int(p.hcx_cnt[dx])
		cxw := int(p.hcx_woff[dx])
		acc = 0
		for k in 0..<cxn {
			acc += u32(p.hcx_w[cxw + k]) * u32(ws.tmp_u[cx0 + k])
		}
		uv := i32((u64(acc) * p.inv_sw2 + (u64(1) << 31)) >> 32)
		acc = 0
		for k in 0..<cxn {
			acc += u32(p.hcx_w[cxw + k]) * u32(ws.tmp_v[cx0 + k])
		}
		vv := i32((u64(acc) * p.inv_sw2 + (u64(1) << 31)) >> 32)

		cc := yv - 16
		dd := uv - 128
		ee := vv - 128
		r := (298 * cc + 409 * ee + 128) >> 8
		g := (298 * cc - 100 * dd - 208 * ee + 128) >> 8
		b := (298 * cc + 516 * dd + 128) >> 8
		o := dx * 4
		out[o + 0] = u8(clamp(r, 0, 255))
		out[o + 1] = u8(clamp(g, 0, 255))
		out[o + 2] = u8(clamp(b, 0, 255))
		out[o + 3] = 255
	}
}

// ScalePool runs scale_row over worker threads with work stealing.
ScalePool :: struct {
	job:     ScaleJob,
	start:   sync.Sema,
	done:    sync.Sema,
	exited:  sync.Sema,
	stopped: i32,
	workers: []^thread.Thread,
}

// scale_worker processes output row bands until the frame is complete.
scale_worker :: proc(data: rawptr) {
	pool := (^ScalePool)(data)
	defer sync.sema_post(&pool.exited)
	ws: ScaleWorkerState
	for {
		sync.sema_wait(&pool.start)
		if sync.atomic_load(&pool.stopped) != 0 {
			return
		}
		scale_prepare_scratch(&ws, pool.job.plan)
		for {
			dy := int(sync.atomic_add(&pool.job.next_dy, 4))
			if dy >= pool.job.plan.dh {
				break
			}
			end := min(dy + 4, pool.job.plan.dh)
			for r in dy..<end {
				scale_row(&ws, &pool.job, r)
			}
		}
		sync.sema_post(&pool.done)
	}
}

// scale_pool_init spawns n permanent scaling workers.
scale_pool_init :: proc(pool: ^ScalePool, n: int) {
	pool.workers = make([]^thread.Thread, n)
	for i in 0..<n {
		pool.workers[i] = thread.create_and_start_with_data(pool, scale_worker, nil, .Normal, true)
	}
}

// scale_pool_stop shuts the scaling workers down and waits for them.
scale_pool_stop :: proc(pool: ^ScalePool) {
	sync.atomic_store(&pool.stopped, 1)
	sync.sema_post(&pool.start, len(pool.workers))
	for _ in 0..<len(pool.workers) {
		sync.sema_wait(&pool.exited)
	}
}

// scale_frame scales one yuv420p frame to rgba using the worker pool.
scale_frame :: proc(pool: ^ScalePool, plan: ^ScalePlan, src_y, src_u, src_v: [^]u8, stride_y, stride_c: int, dst: [^]u8, dst_stride: int) {
	pool.job = ScaleJob{
		plan = plan,
		src_y = src_y,
		src_u = src_u,
		src_v = src_v,
		stride_y = stride_y,
		stride_c = stride_c,
		dst = dst,
		dst_stride = dst_stride,
		next_dy = 0,
	}
	n := len(pool.workers)
	sync.sema_post(&pool.start, n)
	for _ in 0..<n {
		sync.sema_wait(&pool.done)
	}
}
