package aideo

import "core:c"
import "core:fmt"
import "core:os"

// ci_frame decodes one frame of a video file and reports the right edge
// pixels of the source planes and of both scalers.
ci_frame :: proc() {
	if len(os.args) < 3 {
		fmt.printfln("usage: aideo ciframe <video file>")
		return
	}
	path := cstring(strings_clone_to_cstring(os.args[2]))
	av_log_set_level(AV_LOG_QUIET)
	vs := video_open(path)
	if vs == nil {
		fmt.printfln("cannot open %s", os.args[2])
		return
	}
	yf := av_frame_alloc()
	found := false
	for !found {
		if av_read_frame(vs.fmt_ctx, vs.pkt) < 0 {
			break
		}
		if int(vs.pkt.stream_index) != vs.stream_idx {
			av_packet_unref(vs.pkt)
			continue
		}
		avcodec_send_packet(vs.codec_ctx, vs.pkt)
		av_packet_unref(vs.pkt)
		if avcodec_receive_frame(vs.codec_ctx, yf) == 0 {
			found = true
		}
	}
	if !found {
		fmt.printfln("no frame")
		return
	}
	sw := int(yf.width)
	sh := int(yf.height)
	sy := int(yf.linesize[0])
	sc := int(yf.linesize[1])
	fmt.printfln("frame %dx%d format=%d linesize y=%d c=%d", sw, sh, int(yf.format), sy, sc)
	yrow := ([^]u8)(&yf.data[0][(sh / 2) * sy])
	urow := ([^]u8)(&yf.data[1][(sh / 4) * sc])
	vrow := ([^]u8)(&yf.data[2][(sh / 4) * sc])
	fmt.printfln("src Y row end: %3d %3d %3d %3d", yrow[sw - 4], yrow[sw - 3], yrow[sw - 2], yrow[sw - 1])
	fmt.printfln("src U row end: %3d %3d %3d %3d", urow[sw / 2 - 4], urow[sw / 2 - 3], urow[sw / 2 - 2], urow[sw / 2 - 1])
	fmt.printfln("src V row end: %3d %3d %3d %3d", vrow[sw / 2 - 4], vrow[sw / 2 - 3], vrow[sw / 2 - 2], vrow[sw / 2 - 1])

	dw, dh := 2193, 1234
	plan := scale_plan(sw, sh, dw, dh)
	pool: ScalePool
	scale_pool_init(&pool, 8)
	got := make([]u8, dw * dh * 4)
	scale_frame(&pool, plan, yf.data[0], yf.data[1], yf.data[2], sy, sc, &got[0], dw * 4)
	scale_pool_stop(&pool)
	orow := dh / 2 * dw * 4
	fmt.printfln("custom row end: (%3d,%3d,%3d) (%3d,%3d,%3d) (%3d,%3d,%3d)",
		got[orow + (dw - 3) * 4], got[orow + (dw - 3) * 4 + 1], got[orow + (dw - 3) * 4 + 2],
		got[orow + (dw - 2) * 4], got[orow + (dw - 2) * 4 + 1], got[orow + (dw - 2) * 4 + 2],
		got[orow + (dw - 1) * 4], got[orow + (dw - 1) * 4 + 1], got[orow + (dw - 1) * 4 + 2])

	sws_one := sws_getContext(c.int(sw), c.int(sh), AV_PIX_FMT_YUV420P, c.int(dw), c.int(dh), AV_PIX_FMT_RGBA, SWS_FAST_BILINEAR, nil, nil, nil)
	ref := make([]u8, dw * dh * 4)
	sd: [4][^]u8
	sd[0] = yf.data[0]
	sd[1] = yf.data[1]
	sd[2] = yf.data[2]
	sst: [4]c.int
	sst[0] = c.int(sy)
	sst[1] = c.int(sc)
	sst[2] = c.int(sc)
	dpt: [1][^]u8
	dpt[0] = &ref[0]
	dstt: [1]c.int
	dstt[0] = c.int(dw * 4)
	sws_scale(sws_one, &sd[0], &sst[0], 0, c.int(sh), &dpt[0], &dstt[0])
	sws_freeContext(sws_one)
	fmt.printfln("sws    row end: (%3d,%3d,%3d) (%3d,%3d,%3d) (%3d,%3d,%3d)",
		ref[orow + (dw - 3) * 4], ref[orow + (dw - 3) * 4 + 1], ref[orow + (dw - 3) * 4 + 2],
		ref[orow + (dw - 2) * 4], ref[orow + (dw - 2) * 4 + 1], ref[orow + (dw - 2) * 4 + 2],
		ref[orow + (dw - 1) * 4], ref[orow + (dw - 1) * 4 + 1], ref[orow + (dw - 1) * 4 + 2])

	n_bad := 0
	maxd := 0
	for i in 0..<len(ref) {
		d := abs(int(ref[i]) - int(got[i]))
		if d > 4 {
			n_bad += 1
		}
		if d > maxd {
			maxd = d
		}
	}
	fmt.printfln("whole frame: px_over_4=%d max_delta=%d", n_bad, maxd)

	side := 8
	fmt.printfln("col: green_rows(custom) green_rows(sws) delta_rows")
	for x in 0..<side {
		gc := 0
		gs := 0
		dl := 0
		for y in 0..<dh {
			o := y * dw * 4 + x * 4
			if int(got[o + 1]) > int(got[o]) + 20 && int(got[o + 1]) > int(got[o + 2]) + 20 {
				gc += 1
			}
			if int(ref[o + 1]) > int(ref[o]) + 20 && int(ref[o + 1]) > int(ref[o + 2]) + 20 {
				gs += 1
			}
			o2 := y * dw * 4 + (dw - 1 - x) * 4
			if abs(int(got[o2]) - int(ref[o2])) > 4 || abs(int(got[o2 + 1]) - int(ref[o2 + 1])) > 4 || abs(int(got[o2 + 2]) - int(ref[o2 + 2])) > 4 {
				dl += 1
			}
		}
		gc2 := 0
		gs2 := 0
		for y in 0..<dh {
			o := y * dw * 4 + (dw - 1 - x) * 4
			if int(got[o + 1]) > int(got[o]) + 20 && int(got[o + 1]) > int(got[o + 2]) + 20 {
				gc2 += 1
			}
			if int(ref[o + 1]) > int(ref[o]) + 20 && int(ref[o + 1]) > int(ref[o + 2]) + 20 {
				gs2 += 1
			}
		}
		fmt.printfln("left x=%d green custom=%d sws=%d | right x=%d green custom=%d sws=%d delta_rows=%d", x, gc, gs, dw - 1 - x, gc2, gs2, dl)
	}
}
