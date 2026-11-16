package aideo

import "core:c"
import "core:fmt"
import "core:os"
import "core:time"
import "core:sync"
import "core:thread"

COLOR_LEVELS := [7]int{2, 8, 16, 32, 64, 128, 256}

main :: proc() {
	defer prof_report()
	if len(os.args) >= 2 && os.args[1] == "citest" { ci_test(); return }
	if len(os.args) >= 2 && os.args[1] == "cibench" { ci_bench(); return }
	if len(os.args) < 2 {
		fmt.printfln("usage: aideo <video file>")
		os.exit(1)
	}
	path := cstring(strings_clone_to_cstring(os.args[1]))

	av_log_set_level(AV_LOG_QUIET)

	vs := video_open(path)
	if vs == nil {
		fmt.eprintfln("error: cannot open video: %s", os.args[1])
		os.exit(1)
	}

	cols, rows := term_size()
	cell_w, cell_h := term_cell_pixels()
	out_w := cols * cell_w
	out_h := rows * cell_h
	out_w, out_h = fit_aspect(vs.src_w, vs.src_h, out_w, out_h)

	vp := new(VideoPlayer)
	vp.vs = vs
	vp.out_w = out_w
	vp.out_h = out_h

	color_idx := 4
	pal := get_palette(COLOR_LEVELS[color_idx])

	fmt.print("\x1b[?1049h\x1b[?25l\x1b[?1000l\x1b[?1002l\x1b[?1003l")
	fmt.print("\x1b[2J\x1b[3J\x1b[H")

	has_audio := audio_init()
	if has_audio {
		has_audio = audio_start(path)
	}

	term_enter_raw()
	term_watch_winch()
	key_start()
	video_start(vp)

	start_wall := time.now()
	playback_start := start_wall
	pause_start := start_wall
	paused := false
	volume := 1.0
	cache: StripCache
	total_strips := (out_h + 5) / 6
	cache.hashes = make([]u64, total_strips)
	cache.rle = make([][dynamic]u8, total_strips)
	out := Buffer{}
	buffer_init(&out, out_w * out_h / 2)
	pool: StripPool
	strip_pool_init(&pool, 4)
	defer strip_pool_stop(&pool)
	pending: FrameMsg
	have_pending := false
	start_col := 1
	start_row := 1

	quit := false
	_last_frame_t := time.now()
	for !quit {
		_loop_t := time.now()
		prof_add(PROF_FRAME_GAP, i64(time.since(_last_frame_t)))
		_last_frame_t = _loop_t
		for {
			k, ok := key_pop()
			if !ok {
				break
			}
			switch k {
			case KEY_QUIT:
				quit = true
			case KEY_PAUSE:
				paused = !paused
				audio_set_paused(paused)
				if paused {
					pause_start = time.now()
				} else {
					playback_start = time.time_add(playback_start, time.since(pause_start))
				}
			case KEY_VOL_UP:
				volume = min(volume + 0.05, 1.0)
				audio_set_volume(f32(volume))
			case KEY_VOL_DOWN:
				volume = max(volume - 0.05, 0.0)
				audio_set_volume(f32(volume))
			case KEY_COLOR_UP:
				color_idx = min(color_idx + 1, len(COLOR_LEVELS) - 1)
				pal = get_palette(COLOR_LEVELS[color_idx])
				cache.hashes = make([]u64, total_strips)
				cache.rle = make([][dynamic]u8, total_strips)
			case KEY_COLOR_DOWN:
				color_idx = max(color_idx - 1, 0)
				pal = get_palette(COLOR_LEVELS[color_idx])
				cache.hashes = make([]u64, total_strips)
				cache.rle = make([][dynamic]u8, total_strips)
			}
		}

		if term_take_winch() {
			cols, rows = term_size()
			cell_w, cell_h = term_cell_pixels()
			nw, nh := fit_aspect(vs.src_w, vs.src_h, cols*cell_w, rows*cell_h)
			if nw != vp.out_w || nh != vp.out_h {
				if have_pending {
					video_pool_put(vp, pending.buf)
					have_pending = false
				}
				out_w, out_h = nw, nh
				total_strips = (out_h + 5) / 6
				cache.hashes = make([]u64, total_strips)
				cache.rle = make([][dynamic]u8, total_strips)
				video_request_resize(vp, out_w, out_h)
				fmt.print("\x1b[2J\x1b[3J\x1b[H")
			}
		}

		if !have_pending {
			m, ok := fq_try_pop(&vp.queue)
			if ok {
				pending = m
				have_pending = true
			}
		}
		if !have_pending {
			time.sleep(2 * time.Millisecond)
			continue
		}
		if len(pending.buf) != out_w * out_h * 4 {
			video_pool_put(vp, pending.buf)
			have_pending = false
			continue
		}

		pts := pending.pts_sec
		clock := time.duration_seconds(time.since(playback_start))
		if has_audio {
			drift := clock - audio_clock_sec()
			if drift > 0.05 || drift < -0.05 {
				playback_start = time.time_add(time.now(), time.Duration(-audio_clock_sec() * 1e9))
				clock = time.duration_seconds(time.since(playback_start))
			}
		}
		half := 0.020
		if pts > clock + 0.002 {
			remain := pts - clock - 0.001
			if remain > 0 {
				time.sleep(time.Duration(remain * 1e9))
			}
			continue
		}
		if pts < clock - half {
				video_pool_put(vp, pending.buf)
			have_pending = false
			continue
		}

		start_col = max((cols - (out_w + cell_w - 1) / cell_w) / 2 + 1, 1)
		start_row = max((rows - (out_h + cell_h - 1) / cell_h) / 2 + 1, 1)
		buffer_reset(&out)
		write_bytes(&out, []u8{0x1b})
		write_byte(&out, '[')
		write_sixel_num(&out, start_row)
		write_byte(&out, ';')
		write_sixel_num(&out, start_col)
		write_byte(&out, 'H')
		sixel_encode(&pool, &out, pending.buf, out_w, out_h, pal, &cache)
		os.write(os.stdout, out.buf[:out.pos])
	
		video_pool_put(vp, pending.buf)
		have_pending = false
	}

	video_stop(vp)
	audio_shutdown()
	term_leave_raw()
	fmt.print("\x1b[?1049l\x1b[?25h\x1b[?1000h\x1b[?1002h\x1b[?1003h")
}

fit_aspect :: proc(src_w, src_h, max_w, max_h: int) -> (int, int) {
	if src_w <= 0 || src_h <= 0 {
		return max_w, max_h
	}
	w := max_w
	h := w * src_h / src_w
	if h > max_h {
		h = max_h
		w = h * src_w / src_h
	}
	w = max(w, 4)
	h = max(h, 4)
	return w, h
}

strings_clone_to_cstring :: proc(s: string) -> cstring {
	buf := make([]u8, len(s) + 1)
	copy(buf, s)
	return cstring(&buf[0])
}
