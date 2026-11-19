package aideo

import "core:c"
import "core:fmt"
import "core:os"
import "core:sync"
import "core:time"

// cstring_eq compares two NUL terminated strings.
cstring_eq :: proc(a, b: cstring) -> bool {
	pa := ([^]u8)(a)
	pb := ([^]u8)(b)
	i := 0
	for {
		if pa[i] != pb[i] {
			return false
		}
		if pa[i] == 0 {
			return true
		}
		i += 1
	}
}

// is_image_codec_name reports whether a decoder name identifies a still
// image format handled by the photo mode.
is_image_codec_name :: proc(name: cstring) -> bool {
	names := [?]cstring{"png", "mjpeg", "webp", "bmp", "gif", "tiff"}
	for n in names {
		if cstring_eq(name, n) {
			return true
		}
	}
	return false
}

tiff_get16 :: proc(b: []u8, off: int, le: bool) -> int {
	if le {
		return int(b[off]) | int(b[off+1]) << 8
	}
	return int(b[off]) << 8 | int(b[off+1])
}

tiff_get32 :: proc(b: []u8, off: int, le: bool) -> int {
	if le {
		return int(b[off]) | int(b[off+1]) << 8 | int(b[off+2]) << 16 | int(b[off+3]) << 24
	}
	return int(b[off]) << 24 | int(b[off+1]) << 16 | int(b[off+2]) << 8 | int(b[off+3])
}

// tiff_orientation reads the EXIF orientation tag from one TIFF header
// block. The result is between 1 and 8 per the EXIF specification.
tiff_orientation :: proc(seg: []u8) -> int {
	if len(seg) < 8 {
		return 1
	}
	le := seg[0] == 'I' && seg[1] == 'I'
	be := seg[0] == 'M' && seg[1] == 'M'
	if !le && !be {
		return 1
	}
	if tiff_get16(seg, 2, le) != 42 {
		return 1
	}
	ifd := tiff_get32(seg, 4, le)
	if ifd < 0 || ifd + 2 > len(seg) {
		return 1
	}
	n := tiff_get16(seg, ifd, le)
	for k in 0..<n {
		e := ifd + 2 + k * 12
		if e + 12 > len(seg) {
			return 1
		}
		if tiff_get16(seg, e, le) == 0x0112 {
			v := tiff_get16(seg, e + 8, le)
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// photo_orientation_bytes extracts the EXIF orientation from JPEG, PNG
// or WebP container bytes. Missing or invalid tags give 1.
photo_orientation_bytes :: proc(data: []u8) -> int {
	if len(data) >= 4 && data[0] == 0xFF && data[1] == 0xD8 {
		i := 2
		for i + 4 <= len(data) {
			if data[i] != 0xFF {
				return 1
			}
			marker := int(data[i+1])
			if marker == 0xD8 || marker == 0x01 || marker >= 0xD0 && marker <= 0xD7 {
				i += 2
				continue
			}
			if marker == 0xDA || marker == 0xD9 {
				return 1
			}
			seglen := int(data[i+2]) << 8 | int(data[i+3])
			if seglen < 2 {
				return 1
			}
			if marker == 0xE1 && i + 10 <= len(data) {
				p := i + 4
				if data[p] == 'E' && data[p+1] == 'x' && data[p+2] == 'i' && data[p+3] == 'f' {
					e := min(p + 6 + seglen - 2, len(data))
					return tiff_orientation(data[p+6:e])
				}
			}
			i += 2 + seglen
		}
		return 1
	}
	if len(data) >= 8 && data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' {
		i := 8
		for i + 8 <= len(data) {
			clen := int(data[i]) << 24 | int(data[i+1]) << 16 | int(data[i+2]) << 8 | int(data[i+3])
			ctype := data[i+4:i+8]
			if ctype[0] == 'e' && ctype[1] == 'X' && ctype[2] == 'O' && ctype[3] == 'f' {
				s := i + 8
				e := min(s + clen, len(data))
				return tiff_orientation(data[s:e])
			}
			if ctype[0] == 'I' && ctype[1] == 'D' && ctype[2] == 'A' && ctype[3] == 'T' {
				return 1
			}
			i += 8 + clen + 4
		}
		return 1
	}
	if len(data) >= 12 && data[0] == 'R' && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' &&
		data[8] == 'W' && data[9] == 'E' && data[10] == 'B' && data[11] == 'P' {
		i := 12
		for i + 8 <= len(data) {
			ctype := data[i:i+4]
			clen := int(data[i+4]) | int(data[i+5]) << 8 | int(data[i+6]) << 16 | int(data[i+7]) << 24
			if ctype[0] == 'E' && ctype[1] == 'X' && ctype[2] == 'I' && ctype[3] == 'F' {
				s := i + 8
				e := min(s + clen, len(data))
				seg := data[s:e]
				if len(seg) >= 6 && seg[0] == 'E' && seg[1] == 'x' && seg[2] == 'i' && seg[3] == 'f' {
					seg = seg[6:]
				}
				return tiff_orientation(seg)
			}
			i += 8 + clen
			if clen & 1 != 0 {
				i += 1
			}
		}
	}
	return 1
}

// photo_orientation_file reads the file and extracts its EXIF
// orientation. Unreadable files report 1.
photo_orientation_file :: proc(path: string) -> int {
	data, err := os.read_entire_file_from_path(path, context.allocator)
	if err != nil {
		return 1
	}
	defer delete(data)
	return photo_orientation_bytes(data)
}

// photo_swaps_dims reports whether an EXIF orientation exchanges width
// and height.
photo_swaps_dims :: proc(orientation: int) -> bool {
	return orientation >= 5
}

// photo_rotate_plane maps one image plane through the EXIF orientation.
// sw and sh describe the source plane and the destination size follows
// from the orientation.
photo_rotate_plane :: proc(src: [^]u8, sw, sh, ss: int, dst: [^]u8, ds: int, orientation: int) {
	dw := sw
	dh := sh
	if photo_swaps_dims(orientation) {
		dw, dh = sh, sw
	}
	for dy in 0..<dh {
		for dx in 0..<dw {
			sx: int
			sy: int
			switch orientation {
			case 1:
				sx, sy = dx, dy
			case 2:
				sx, sy = sw - 1 - dx, dy
			case 3:
				sx, sy = sw - 1 - dx, sh - 1 - dy
			case 4:
				sx, sy = dx, sh - 1 - dy
			case 5:
				sx, sy = dy, dx
			case 6:
				sx, sy = dy, sh - 1 - dx
			case 7:
				sx, sy = sw - 1 - dy, sh - 1 - dx
			case 8:
				sx, sy = sw - 1 - dy, dx
			}
			dst[dy * ds + dx] = src[sy * ss + sx]
		}
	}
}

// is_animation_codec_name reports decoder names of looping animations
// that play through the video pipeline.
is_animation_codec_name :: proc(name: cstring) -> bool {
	names := [?]cstring{"apng", "webp_anim"}
	for n in names {
		if cstring_eq(name, n) {
			return true
		}
	}
	return false
}

// webp_animation_probe reports whether WebP container bytes carry the
// VP8X animation flag.
webp_animation_probe :: proc(data: []u8) -> bool {
	if len(data) < 24 {
		return false
	}
	if data[0] != 'R' || data[1] != 'I' || data[2] != 'F' || data[3] != 'F' {
		return false
	}
	if data[8] != 'W' || data[9] != 'E' || data[10] != 'B' || data[11] != 'P' {
		return false
	}
	if data[12] != 'V' || data[13] != 'P' || data[14] != '8' || data[15] != 'X' {
		return false
	}
	return data[20] & 0x02 != 0
}

// stream_is_animated reports whether an image stream carries motion and
// should play as a looping video instead of a still picture.
stream_is_animated :: proc(vs: ^VideoStream, path: string) -> bool {
	iname := vs.fmt_ctx.iformat.name
	if cstring_eq(iname, "gif") || cstring_eq(iname, "apng") {
		return true
	}
	if cstring_eq(iname, "webp_pipe") {
		data, err := os.read_entire_file_from_path(path, context.allocator)
		if err != nil {
			return false
		}
		defer delete(data)
		return webp_animation_probe(data)
	}
	return false
}

// Photo holds one still image as an upright yuv420p frame, ready for
// the shared scaling and sixel pipeline.
Photo :: struct {
	y:         []u8,
	u:         []u8,
	v:         []u8,
	w:         int,
	h:         int,
	stride_y:  int,
	stride_c:  int,
	codec:     string,
}

// photo_release frees the decoded plane storage.
photo_release :: proc(ph: ^Photo) {
	delete(ph.y)
	delete(ph.u)
	delete(ph.v)
}

// photo_decode decodes the first frame of one still image file into
// upright yuv420p planes. The decoder name is kept for the status label.
photo_decode :: proc(path: cstring, raw_path: string) -> (Photo, bool) {
	ph: Photo
	fmt_ctx: ^AVFormatContext
	if avformat_open_input(&fmt_ctx, path, nil, nil) != 0 {
		return ph, false
	}
	if avformat_find_stream_info(fmt_ctx, nil) < 0 {
		avformat_close_input(&fmt_ctx)
		return ph, false
	}
	idx := av_find_best_stream(fmt_ctx, AVMEDIA_TYPE_VIDEO, -1, -1, nil, 0)
	if idx < 0 {
		avformat_close_input(&fmt_ctx)
		return ph, false
	}
	par := fmt_ctx.streams[idx].codecpar
	codec_id := par.codec_id
	dec := avcodec_find_decoder(codec_id)
	if dec == nil {
		avformat_close_input(&fmt_ctx)
		return ph, false
	}
	codec_ctx := avcodec_alloc_context3(dec)
	if avcodec_parameters_to_context(codec_ctx, par) < 0 {
		avcodec_free_context(&codec_ctx)
		avformat_close_input(&fmt_ctx)
		return ph, false
	}
	if avcodec_open2(codec_ctx, dec, nil) < 0 {
		avcodec_free_context(&codec_ctx)
		avformat_close_input(&fmt_ctx)
		return ph, false
	}
	pkt := av_packet_alloc()
	frame := av_frame_alloc()
	found := false
	for {
		if av_read_frame(fmt_ctx, pkt) < 0 {
			break
		}
		if int(pkt.stream_index) != int(idx) {
			av_packet_unref(pkt)
			continue
		}
		avcodec_send_packet(codec_ctx, pkt)
		av_packet_unref(pkt)
		if avcodec_receive_frame(codec_ctx, frame) == 0 {
			found = true
			break
		}
	}

	orientation := photo_orientation_file(raw_path)
	w := int(frame.width)
	h := int(frame.height)
	sw2 := (w + 1) / 2
	sh2 := (h + 1) / 2
	ow, oh := w, h
	if photo_swaps_dims(orientation) {
		ow, oh = h, w
	}
	osw2 := (ow + 1) / 2
	osh2 := (oh + 1) / 2
	ph.w, ph.h = ow, oh
	ph.stride_y = ow
	ph.stride_c = osw2
	ph.codec = string(avcodec_get_name(codec_id))

	ybuf: [^]u8
	ubuf: [^]u8
	vbuf: [^]u8
	if found && int(frame.format) == AV_PIX_FMT_YUV420P {
		ybuf = frame.data[0]
		ubuf = frame.data[1]
		vbuf = frame.data[2]
		ph.y = make([]u8, ow * oh)
		ph.u = make([]u8, osw2 * osh2)
		ph.v = make([]u8, osw2 * osh2)
		photo_rotate_plane(ybuf, w, h, int(frame.linesize[0]), &ph.y[0], ow, orientation)
		photo_rotate_plane(ubuf, sw2, sh2, int(frame.linesize[1]), &ph.u[0], osw2, orientation)
		photo_rotate_plane(vbuf, sw2, sh2, int(frame.linesize[2]), &ph.v[0], osw2, orientation)
	} else if found {
		conv := make([]u8, w * h + sw2 * sh2 * 2)
		defer delete(conv)
		sws := sws_getContext(c.int(w), c.int(h), frame.format,
			c.int(w), c.int(h), AV_PIX_FMT_YUV420P, SWS_BILINEAR, nil, nil, nil)
		if sws == nil {
			found = false
		} else {
			dst_data: [3][^]u8
			dst_data[0] = &conv[0]
			dst_data[1] = &conv[w * h]
			dst_data[2] = &conv[w * h + sw2 * sh2]
			dst_stride: [3]c.int
			dst_stride[0] = c.int(w)
			dst_stride[1] = c.int(sw2)
			dst_stride[2] = c.int(sw2)
			sws_scale(sws, &frame.data[0], &frame.linesize[0], 0, c.int(h), &dst_data[0], &dst_stride[0])
			sws_freeContext(sws)
			ph.y = make([]u8, ow * oh)
			ph.u = make([]u8, osw2 * osh2)
			ph.v = make([]u8, osw2 * osh2)
			photo_rotate_plane(dst_data[0], w, h, w, &ph.y[0], ow, orientation)
			photo_rotate_plane(dst_data[1], sw2, sh2, sw2, &ph.u[0], osw2, orientation)
			photo_rotate_plane(dst_data[2], sw2, sh2, sw2, &ph.v[0], osw2, orientation)
		}
	}

	av_packet_free(&pkt)
	av_frame_free(&frame)
	avcodec_free_context(&codec_ctx)
	avformat_close_input(&fmt_ctx)
	if !found {
		photo_release(&ph)
	}
	return ph, found
}

// photo_run displays one still image until quit. It shares the scaler,
// palette, sixel encoder and terminal handling with the video mode.
photo_run :: proc(path: cstring, raw_path: string, full: bool) {
	ph, ok := photo_decode(path, raw_path)
	if !ok {
		fmt.eprintfln("error: cannot open image: %s", raw_path)
		os.exit(1)
	}

	fmt.print("\x1b[?1049h\x1b[?25l\x1b[?1004h\x1b[?1000l\x1b[?1002l\x1b[?1003l")
	fmt.print("\x1b[2J\x1b[3J\x1b[H")

	term_enter_raw()
	term_watch_winch()
	key_start()

	cols, rows := term_size()
	cell_w, cell_h := term_cell_pixels()
	full_disp := full
	out_w, out_h := fit_disp(ph.w, ph.h, cols, rows, cell_w, cell_h, full_disp)
	start_col := max((cols - (out_w + cell_w - 1) / cell_w) / 2 + 1, 1)
	start_row := max((rows - (out_h + cell_h - 1) / cell_h) / 2 + 1, 1)

	vp := new(VideoPlayer)
	rc: RenderCtx
	rc.vp = vp
	pool: StripPool
	strip_pool_init(&pool, max(cpu_count() / 2, 4))
	rc.pool = &pool
	for i in 0..<len(rc.bufs) {
		buffer_init(&rc.bufs[i], out_w * out_h / 2)
	}
	render_start(&rc)
	spool: ScalePool
	scale_pool_init(&spool, max(cpu_count() / 2, 2))

	color_idx := len(PALETTE_LEVELS) - 1
	pal := get_palette(PALETTE_LEVELS[color_idx])
	plan := scale_plan(ph.w, ph.h, out_w, out_h)

	need_render := true
	show_fps := false
	label_t := time.Time{}
	full_t := time.now()
	fps_ui: FpsUI
	quit := false
	for !quit {
		for {
			k, ok2 := key_pop()
			if !ok2 {
				break
			}
			switch k {
			case KEY_QUIT:
				quit = true
			case KEY_SIZE:
				full_disp = !full_disp
				sync.atomic_store(&winch_flag, 1)
			case KEY_COLOR_UP:
				color_idx = min(color_idx + 1, len(PALETTE_LEVELS) - 1)
				pal = get_palette(PALETTE_LEVELS[color_idx])
				need_render = true
			case KEY_COLOR_DOWN:
				color_idx = max(color_idx - 1, 0)
				pal = get_palette(PALETTE_LEVELS[color_idx])
				need_render = true
			case KEY_FOCUS_IN:
				render_submit(&rc, RenderJob{clear = true})
				need_render = true
			case KEY_FPS:
				show_fps = !show_fps
				label_t = time.Time{}
				if !show_fps {
					sync.mutex_lock(&rc.stdout_mu)
					fps_ui_hide(&fps_ui)
					sync.mutex_unlock(&rc.stdout_mu)
				}
			}
		}

		if term_take_winch() {
			cols, rows = term_size()
			cell_w, cell_h = term_cell_pixels()
			nw, nh := fit_disp(ph.w, ph.h, cols, rows, cell_w, cell_h, full_disp)
			if nw != out_w || nh != out_h {
				out_w, out_h = nw, nh
				start_col = max((cols - (out_w + cell_w - 1) / cell_w) / 2 + 1, 1)
				start_row = max((rows - (out_h + cell_h - 1) / cell_h) / 2 + 1, 1)
				fps_ui.prev_len = 0
			}
			render_submit(&rc, RenderJob{clear = true})
			need_render = true
		}

		if time.duration_seconds(time.since(label_t)) >= 0.25 {
			label_t = time.now()
			if show_fps {
				label_row := start_row + (out_h + cell_h - 1) / cell_h + 1
				size_mark := "\u2922"
				if !full_disp {
					size_mark = "\u26F6"
				}
				label := fmt.tprintf("[ %d | %dx%d %s | %s ]",
					pal.nc, ph.w, ph.h, ph.codec, size_mark)
				sync.mutex_lock(&rc.stdout_mu)
				if label_row <= rows {
					fps_ui_show(&fps_ui, label_row, cols, label)
				} else {
					fps_ui_hide(&fps_ui)
				}
				sync.mutex_unlock(&rc.stdout_mu)
			}
		}

		if time.duration_seconds(time.since(full_t)) >= 2.0 {
			full_t = time.now()
			need_render = true
		}

		if need_render {
			need_render = false
			scale_plan_free(plan)
			plan = scale_plan(ph.w, ph.h, out_w, out_h)
			buf := video_pool_get(vp, out_w * out_h * 4)
			scale_frame(&spool, plan, &ph.y[0], &ph.u[0], &ph.v[0],
				ph.stride_y, ph.stride_c, &buf[0], out_w * 4)
			render_submit(&rc, RenderJob{
				buf = buf,
				w = out_w,
				h = out_h,
				start_col = start_col,
				start_row = start_row,
				pal = pal,
				reset_cache = true,
			})
			continue
		}
		time.sleep(5 * time.Millisecond)
	}

	render_stop(&rc)
	if len(rc.freeze) > 0 {
		video_pool_put(vp, rc.freeze)
		rc.freeze = nil
	}
	scale_pool_stop(&spool)
	strip_pool_stop(&pool)
	scale_plan_free(plan)
	sync.mutex_lock(&vp.pool_mu)
	for i in 0..<len(vp.pool) {
		if vp.pool[i] != nil {
			free(&vp.pool[i][0])
			vp.pool[i] = nil
		}
	}
	sync.mutex_unlock(&vp.pool_mu)
	for i in 0..<len(rc.bufs) {
		if len(rc.bufs[i].buf) > 0 {
			free(&rc.bufs[i].buf[0])
		}
	}
	photo_release(&ph)
	free(vp)
	term_leave_raw()
	fmt.print("\x1b[?1049l\x1b[?25h\x1b[?1004l\x1b[?1000h\x1b[?1002h\x1b[?1003h")
}
