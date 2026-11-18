package aideo

import "core:c"
import "core:fmt"
import "core:time"

SwsCase :: struct {
	dw, dh, slices: int,
	band_ctx: bool,
}

// ci_sws checks whether scaling can be split across independent scaler
// contexts and still produce byte identical output.
ci_sws :: proc() {
	sw, sh := 3840, 2160
	ybuf := make([]u8, sw * sh)
	ubuf := make([]u8, sw / 2 * sh / 2)
	vbuf := make([]u8, sw / 2 * sh / 2)
	for y in 0..<sh {
		for x in 0..<sw {
			ybuf[y * sw + x] = u8((x * 255 / sw + y * 255 / sh) / 2)
		}
	}
	for y in 0..<sh / 2 {
		for x in 0..<sw / 2 {
			ubuf[y * (sw / 2) + x] = u8(64 + x * 128 / (sw / 2))
			vbuf[y * (sw / 2) + x] = u8(192 - y * 128 / (sh / 2))
		}
	}
	src_data: [4][^]u8
	src_data[0] = &ybuf[0]
	src_data[1] = &ubuf[0]
	src_data[2] = &vbuf[0]
	src_stride: [4]c.int
	src_stride[0] = c.int(sw)
	src_stride[1] = c.int(sw / 2)
	src_stride[2] = c.int(sw / 2)

	scale_all := proc(dw, dh, slices: int, band_ctx: bool, ybuf, ubuf, vbuf: [^]u8, sw, sh: int) -> []u8 {
		dst := make([]u8, dw * dh * 4)
		src_data: [4][^]u8
		src_data[0] = ybuf
		src_data[1] = ubuf
		src_data[2] = vbuf
		src_stride: [4]c.int
		src_stride[0] = c.int(sw)
		src_stride[1] = c.int(sw / 2)
		src_stride[2] = c.int(sw / 2)
		dst_stride: [1]c.int
		dst_stride[0] = c.int(dw * 4)
		for k in 0..<slices {
			sy0 := k * sh / slices
			sy1 := (k + 1) * sh / slices
			dy0 := k * dh / slices
			dy1 := (k + 1) * dh / slices
			ctx: ^SwsContext
			if band_ctx {
				ctx = sws_getContext(c.int(sw), c.int(sy1 - sy0), AV_PIX_FMT_YUV420P,
					c.int(dw), c.int(dy1 - dy0), AV_PIX_FMT_RGBA, SWS_FAST_BILINEAR, nil, nil, nil)
			} else {
				ctx = sws_getContext(c.int(sw), c.int(sh), AV_PIX_FMT_YUV420P,
					c.int(dw), c.int(dh), AV_PIX_FMT_RGBA, SWS_FAST_BILINEAR, nil, nil, nil)
			}
			sd: [4][^]u8
			sd[0] = &ybuf[sy0 * sw]
			sd[1] = &ubuf[(sy0 / 2) * (sw / 2)]
			sd[2] = &vbuf[(sy0 / 2) * (sw / 2)]
			dptr: [1][^]u8
			if band_ctx {
				dptr[0] = &dst[dy0 * dw * 4]
				sws_scale(ctx, &sd[0], &src_stride[0], 0, c.int(sy1 - sy0), &dptr[0], &dst_stride[0])
			} else {
				dptr[0] = &dst[0]
				sws_scale(ctx, &sd[0], &src_stride[0], c.int(sy0), c.int(sy1 - sy0), &dptr[0], &dst_stride[0])
			}
			sws_freeContext(ctx)
		}
		return dst
	}

	cases := [6]SwsCase{
		{2193, 1234, 1, false},
		{2193, 1234, 2, false},
		{2193, 1234, 4, false},
		{2193, 1234, 2, true},
		{2193, 1234, 4, true},
		{1097, 617, 2, true},
	}
	fmt.printfln("--- color probe solid patches ---")
	probes := [6][3]u8{
		{128, 128, 128},
		{235, 128, 128},
		{16, 128, 128},
		{126, 90, 240},
		{60, 120, 200},
		{200, 160, 60},
	}
	for pr in probes {
		py := make([]u8, 64 * 64)
		pu := make([]u8, 32 * 32)
		pv := make([]u8, 32 * 32)
		for i in 0..<len(py) {
			py[i] = pr[0]
		}
		for i in 0..<len(pu) {
			pu[i] = pr[1]
			pv[i] = pr[2]
		}
		ref := scale_all(64, 64, 1, false, &py[0], &pu[0], &pv[0], 64, 64)
		plan := scale_plan(64, 64, 64, 64)
		pool: ScalePool
		scale_pool_init(&pool, 1)
		got := make([]u8, 64 * 64 * 4)
		scale_frame(&pool, plan, &py[0], &pu[0], &pv[0], 64, 32, &got[0], 256)
		scale_pool_stop(&pool)
		fmt.printfln("YUV=(%3d,%3d,%3d) sws=(%3d,%3d,%3d) custom=(%3d,%3d,%3d)",
			pr[0], pr[1], pr[2], ref[0], ref[1], ref[2], got[0], got[1], got[2])
	}

	for tc in cases {
		ref := scale_all(tc.dw, tc.dh, 1, false, &ybuf[0], &ubuf[0], &vbuf[0], sw, sh)
		got := scale_all(tc.dw, tc.dh, tc.slices, tc.band_ctx, &ybuf[0], &ubuf[0], &vbuf[0], sw, sh)
		mark := "OK"
		diff_n := 0
		diff_max := 0
		diff_rows: [dynamic]int
		if len(ref) != len(got) {
			mark = "DIFF"
		} else {
			for i in 0..<len(ref) {
				if ref[i] != got[i] {
					mark = "DIFF"
					diff_n += 1
					d := abs(int(ref[i]) - int(got[i]))
					if d > diff_max {
						diff_max = d
					}
					row := i / (tc.dw * 4)
					if len(diff_rows) == 0 || diff_rows[len(diff_rows) - 1] != row {
						append(&diff_rows, row)
					}
				}
			}
		}
		fmt.printfln("sws %4dx%-4d slices=%d band_ctx=%v %s diff_px=%d max_delta=%d rows=%v", tc.dw, tc.dh, tc.slices, tc.band_ctx, mark, diff_n, diff_max, diff_rows)
		clear(&diff_rows)
	}

	fmt.printfln("--- fast vs scalar reference parity ---")
	pcases := [2]SwsCase{{2193, 1234, 0, false}, {1097, 617, 0, false}}
	for tc in pcases {
		plan := scale_plan(sw, sh, tc.dw, tc.dh)
		pool: ScalePool
		scale_pool_init(&pool, 8)
		got := make([]u8, tc.dw * tc.dh * 4)
		scale_frame(&pool, plan, &ybuf[0], &ubuf[0], &vbuf[0], sw, sw / 2, &got[0], tc.dw * 4)
		scale_pool_stop(&pool)
		ref := make([]u8, tc.dw * tc.dh * 4)
		ws: ScaleWorkerState
		scale_prepare_scratch(&ws, plan)
		job := ScaleJob{
			plan = plan,
			src_y = &ybuf[0],
			src_u = &ubuf[0],
			src_v = &vbuf[0],
			stride_y = sw,
			stride_c = sw / 2,
			dst = &ref[0],
			dst_stride = tc.dw * 4,
		}
		for dy in 0..<tc.dh {
			scale_row_ref(&ws, &job, dy)
		}
		n_diff := 0
		maxd := 0
		shown := 0
		for i in 0..<len(ref) {
			if ref[i] != got[i] {
				n_diff += 1
				d := abs(int(ref[i]) - int(got[i]))
				if d > maxd {
					maxd = d
				}
				if shown < 5 {
					fmt.printfln("  first diffs i=%d ref=%d got=%d", i, ref[i], got[i])
					shown += 1
				}
			}
		}
		fmt.printfln("parity %4dx%-4d diffs=%d maxd=%d %s", tc.dw, tc.dh, n_diff, maxd, n_diff == 0 ? "OK" : "DIFF")
	}

	fmt.printfln("--- custom scaler vs sws ---")
	cases2 := [2]SwsCase{{2193, 1234, 0, false}, {1097, 617, 0, false}}
	for tc in cases2 {
		ref := scale_all(tc.dw, tc.dh, 1, false, &ybuf[0], &ubuf[0], &vbuf[0], sw, sh)
		plan := scale_plan(sw, sh, tc.dw, tc.dh)
		pool: ScalePool
		scale_pool_init(&pool, 8)
		got := make([]u8, tc.dw * tc.dh * 4)
		scale_frame(&pool, plan, &ybuf[0], &ubuf[0], &vbuf[0], sw, sw / 2, &got[0], tc.dw * 4)
		sum: i64
		maxd := 0
		n_bad := 0
		for i in 0..<len(ref) {
			d := abs(int(ref[i]) - int(got[i]))
			sum += i64(d)
			if d > maxd {
				maxd = d
			}
			if d > 4 {
				n_bad += 1
			}
		}
		fmt.printfln("custom %4dx%-4d mean_delta=%.3f max_delta=%d px_over_4=%d", tc.dw, tc.dh, f64(sum) / f64(len(ref)), maxd, n_bad)
		for reps in 0..<3 {
			tt := time.now()
			scale_frame(&pool, plan, &ybuf[0], &ubuf[0], &vbuf[0], sw, sw / 2, &got[0], tc.dw * 4)
			fmt.printfln("scale_frame 8 workers: %.2fms", time.duration_seconds(time.since(tt)) * 1000)
		}
		tt := time.now()
		sws_one := sws_getContext(c.int(sw), c.int(sh), AV_PIX_FMT_YUV420P, c.int(tc.dw), c.int(tc.dh), AV_PIX_FMT_RGBA, SWS_FAST_BILINEAR, nil, nil, nil)
		sd: [4][^]u8
		sd[0] = &ybuf[0]
		sd[1] = &ubuf[0]
		sd[2] = &vbuf[0]
		sst: [4]c.int
		sst[0] = c.int(sw)
		sst[1] = c.int(sw / 2)
		sst[2] = c.int(sw / 2)
		dpt: [1][^]u8
		dpt[0] = &got[0]
		dstt: [1]c.int
		dstt[0] = c.int(tc.dw * 4)
		sws_scale(sws_one, &sd[0], &sst[0], 0, c.int(sh), &dpt[0], &dstt[0])
		fmt.printfln("sws single           : %.2fms", time.duration_seconds(time.since(tt)) * 1000)
		sws_freeContext(sws_one)
		scale_pool_stop(&pool)
		scale_plan_free(plan)
	}
}
