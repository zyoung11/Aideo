package aideo

import "core:c"
import "core:fmt"
import "core:os"
import "core:time"
import "core:sync"
import "core:thread"

COLOR_LEVELS := [7]int{2, 8, 16, 32, 64, 128, 256}

main :: proc() {
	// defer prof_report()
	if len(os.args) >= 2 && os.args[1] == "citest" { ci_test(); return }
	if len(os.args) >= 2 && os.args[1] == "cisws" { ci_sws(); return }
	if len(os.args) >= 2 && os.args[1] == "ciframe" { ci_frame(); return }
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
	muted := false
	pause_clock := 0.0
	volume := 1.0
	cache := new(StripCache)
	total_strips := (out_h + 5) / 6
	strip_cache_init(cache, total_strips)
	pool: StripPool
	strip_pool_init(&pool, max(cpu_count() / 2, 4))
	defer strip_pool_stop(&pool)
	rc: RenderCtx
	rc.vp = vp
	rc.pool = &pool
	for i in 0..<len(rc.bufs) {
		buffer_init(&rc.bufs[i], out_w * out_h / 2)
	}
	render_start(&rc)
	pending: FrameMsg
	have_pending := false
	force_frame := false
	start_col := 1
	start_row := 1
	show_fps := false
	last_vol_t: time.Time
	fps_ui: FpsUI
	src_fps := vs.fps
	fps_sample_t := time.now()
	fps_sample_n := 0
	fps_ui_t := time.Time{}

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
					pause_clock = time.duration_seconds(time.since(playback_start))
				} else {
					playback_start = time.time_add(playback_start, time.since(pause_start))
				}
			case KEY_VOL_UP:
				volume = min(volume + 0.05, 1.0)
				muted = false
				audio_set_volume(f32(volume))
				last_vol_t = time.now()
				fps_ui_t = time.Time{}
			case KEY_VOL_DOWN:
				volume = max(volume - 0.05, 0.0)
				muted = false
				audio_set_volume(f32(volume))
				last_vol_t = time.now()
				fps_ui_t = time.Time{}
			case KEY_MUTE:
				muted = !muted
				if muted {
					audio_set_volume(0.0)
				} else {
					audio_set_volume(f32(volume))
				}
				last_vol_t = time.now()
				fps_ui_t = time.Time{}
			case KEY_COLOR_UP:
				color_idx = min(color_idx + 1, len(COLOR_LEVELS) - 1)
				pal = get_palette(COLOR_LEVELS[color_idx])
				cache = new(StripCache)
				strip_cache_init(cache, total_strips)
				if paused {
					rerender_freeze(&rc, out_w, out_h, start_col, start_row, pal, cache)
				}
			case KEY_COLOR_DOWN:
				color_idx = max(color_idx - 1, 0)
				pal = get_palette(COLOR_LEVELS[color_idx])
				cache = new(StripCache)
				strip_cache_init(cache, total_strips)
				if paused {
					rerender_freeze(&rc, out_w, out_h, start_col, start_row, pal, cache)
				}
			case KEY_FPS:
				show_fps = !show_fps
				fps_ui_t = time.Time{}
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
			nw, nh := fit_aspect(vs.src_w, vs.src_h, cols*cell_w, rows*cell_h)
			if nw != vp.out_w || nh != vp.out_h {
				if have_pending {
					video_pool_put(vp, pending.buf)
					have_pending = false
				}
				out_w, out_h = nw, nh
				total_strips = (out_h + 5) / 6
				start_col = max((cols - (out_w + cell_w - 1) / cell_w) / 2 + 1, 1)
				start_row = max((rows - (out_h + cell_h - 1) / cell_h) / 2 + 1, 1)
				cache = new(StripCache)
				strip_cache_init(cache, total_strips)
				video_request_resize(vp, out_w, out_h)
				render_submit(&rc, RenderJob{clear = true})
				force_frame = true
				fps_ui.prev_len = 0
			}
		}

		if time.duration_seconds(time.since(fps_ui_t)) >= 0.25 {
			fps_ui_t = time.now()
			wn := int(sync.atomic_load(&rc.written))
			el := time.duration_seconds(time.since(fps_sample_t))
			inst := 0.0
			if el > 0 {
				inst = f64(wn - fps_sample_n) / el
			}
			fps_sample_t = time.now()
			fps_sample_n = wn
			if show_fps {
				fps_row := start_row + (out_h + cell_h - 1) / cell_h
				label := fmt.tprintf("[ %d | %.3f/%sfps ]", pal.nc, inst, fps_preset(src_fps))
				if time.duration_seconds(time.since(last_vol_t)) < 5.0 {
					seg := fmt.tprintf("%d%%", int(volume * 100 + 0.5))
					if muted {
						seg = "MUTE"
					}
					label = fmt.tprintf("[ %d | %.3f/%sfps | %s ]", pal.nc, inst, fps_preset(src_fps), seg)
				}
				sync.mutex_lock(&rc.stdout_mu)
				if fps_row <= rows {
					fps_ui_show(&fps_ui, fps_row, cols, label)
				} else {
					fps_ui_hide(&fps_ui)
				}
				sync.mutex_unlock(&rc.stdout_mu)
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

		start_col = max((cols - (out_w + cell_w - 1) / cell_w) / 2 + 1, 1)
		start_row = max((rows - (out_h + cell_h - 1) / cell_h) / 2 + 1, 1)
		if force_frame {
			force_frame = false
			render_submit(&rc, RenderJob{
				buf = pending.buf,
				pts_sec = pending.pts_sec,
				w = out_w,
				h = out_h,
				start_col = start_col,
				start_row = start_row,
				pal = pal,
				cache = cache,
			})
			have_pending = false
			continue
		}

		pts := pending.pts_sec
		clock := pause_clock
		if !paused {
			clock = time.duration_seconds(time.since(playback_start))
			if has_audio {
				drift := clock - audio_clock_sec()
				if drift > 0.05 || drift < -0.05 {
					playback_start = time.time_add(time.now(), time.Duration(-audio_clock_sec() * 1e9))
					clock = time.duration_seconds(time.since(playback_start))
				}
			}
		}
		lag_tol := 0.100
		if src_fps > 0 {
			lag_tol = 2.0 / src_fps
		}
		if pts > clock + 0.002 {
			remain := pts - clock - 0.001
			if remain > 0 {
				time.sleep(time.Duration(remain * 1e9))
			}
			continue
		}
		if pts < clock - lag_tol {
			video_pool_put(vp, pending.buf)
			have_pending = false
			continue
		}

		start_col = max((cols - (out_w + cell_w - 1) / cell_w) / 2 + 1, 1)
		start_row = max((rows - (out_h + cell_h - 1) / cell_h) / 2 + 1, 1)
		render_submit(&rc, RenderJob{
			buf = pending.buf,
			pts_sec = pts,
			w = out_w,
			h = out_h,
			start_col = start_col,
			start_row = start_row,
			pal = pal,
			cache = cache,
		})
		have_pending = false
	}

	render_stop(&rc)
	video_stop(vp)
	audio_shutdown()
	term_leave_raw()
	fmt.print("\x1b[?1049l\x1b[?25h\x1b[?1000h\x1b[?1002h\x1b[?1003h")
}

// fps_preset formats a frame rate with conventional notation for the
// common rates and falls back to three decimals for anything else.
fps_preset :: proc(fps: f64) -> string {
	values := [8]f64{23.976, 24, 25, 29.97, 30, 50, 59.94, 60}
	names := [8]string{"23.976", "24", "25", "29.97", "30", "50", "59.94", "60"}
	for i in 0..<8 {
		if abs(fps - values[i]) < 0.005 {
			return names[i]
		}
	}
	return fmt.tprintf("%.3f", fps)
}

FpsUI :: struct {
	prev_row: int,
	prev_col: int,
	prev_len: int,
}

// fps_ui_hide erases the label drawn by the previous fps_ui_show call.
fps_ui_hide :: proc(ui: ^FpsUI) {
	if ui.prev_len > 0 {
		spaces := "                                        "
		n := min(ui.prev_len, len(spaces))
		fmt.printf("\x1b[%d;%dH%s", ui.prev_row, ui.prev_col, spaces[:n])
		ui.prev_len = 0
	}
}

// fps_ui_show_at erases the previous label and draws bold text at a fixed
// column.
fps_ui_show_at :: proc(ui: ^FpsUI, row, col: int, text: string) {
	fps_ui_hide(ui)
	fmt.printf("\x1b[%d;%dH\x1b[1m%s\x1b[0m", row, col, text)
	ui.prev_row = row
	ui.prev_col = col
	ui.prev_len = len(text)
}

// fps_ui_show erases the previous label and draws bold text centered on row.
fps_ui_show :: proc(ui: ^FpsUI, row, cols: int, text: string) {
	fps_ui_show_at(ui, row, max((cols - len(text)) / 2 + 1, 1), text)
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
