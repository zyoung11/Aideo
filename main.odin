package aideo

import "core:c"
import "core:fmt"
import "core:os"
import "core:strconv"
import "core:time"
import "core:sync"
import "core:thread"

main :: proc() {
	if len(os.args) >= 2 && os.args[1] == "citest" { ci_test(); return }
	if len(os.args) >= 2 && os.args[1] == "cisws" { ci_sws(); return }
	if len(os.args) >= 2 && os.args[1] == "ciframe" { ci_frame(); return }
	if len(os.args) >= 2 && os.args[1] == "cibench" { ci_bench(); return }
	color_cli := 0
	vol_cli := -1.0
	start_sec := 0.0
	full_cli := true
	profile_cli := false
	file_arg := ""
	argi := 1
	for argi < len(os.args) {
		a := os.args[argi]
		if a == "-h" || a == "--help" || a == "help" {
			print_help()
			return
		}
		if a == "--version" || a == "-V" {
			print_version()
			return
		}
		if a == "-p" || a == "--profile" {
			profile_cli = true
		} else if a == "-c" || a == "--colors" {
			argi += 1
			if argi >= len(os.args) {
				cli_error("missing value for --colors")
				os.exit(1)
			}
			v, ok := strconv.parse_int(os.args[argi])
			color_cli = int(v)
			if !ok {
				cli_error("invalid color count")
				os.exit(1)
			}
		} else if a == "-v" || a == "--volume" {
			argi += 1
			if argi >= len(os.args) {
				cli_error("missing value for --volume")
				os.exit(1)
			}
			v, ok := strconv.parse_f64(os.args[argi])
			vol_cli = v
			if !ok || vol_cli < 0.0 || vol_cli > 100.0 {
				cli_error("volume must be from 0 to 100")
				os.exit(1)
			}
		} else if a == "-s" || a == "--size" {
			argi += 1
			if argi >= len(os.args) {
				cli_error("missing value for --size")
				os.exit(1)
			}
			if os.args[argi] == "half" {
				full_cli = false
			} else if os.args[argi] != "full" {
				cli_error("size must be full or half")
				os.exit(1)
			}
		} else if a == "-t" || a == "--start" {
			argi += 1
			if argi >= len(os.args) {
				cli_error("missing value for --start")
				os.exit(1)
			}
			v, ok := parse_time(os.args[argi])
			start_sec = v
			if !ok {
				cli_error("start time must be seconds or mm:ss or hh:mm:ss")
				os.exit(1)
			}
		} else if len(a) > 0 && a[0] == '-' {
			cli_error(fmt.tprintf("unknown option %s", a))
			os.exit(1)
		} else {
			file_arg = a
		}
		argi += 1
	}
	if file_arg == "" {
		print_help()
		os.exit(1)
	}
	if color_cli != 0 {
		found := false
		for lv in PALETTE_LEVELS {
			if lv == color_cli {
				found = true
			}
		}
		if !found {
			cli_error("color count must be one of 2 8 16 32 64 128 256")
			os.exit(1)
		}
	}
	path := strings_clone_to_cstring(file_arg)

	av_log_set_level(AV_LOG_QUIET)

	vs := video_open(path)
	if vs == nil {
		fmt.eprintfln("error: cannot open video: %s", file_arg)
		os.exit(1)
	}
	if is_image_codec_name(vs.codec_name) {
		if stream_is_animated(vs, file_arg) {
			vs.loop_forever = true
		} else {
			video_close_stream(vs)
			photo_run(path, file_arg, full_cli)
			return
		}
	} else if is_animation_codec_name(vs.codec_name) {
		vs.loop_forever = true
	}

	cols, rows := term_size()
	cell_w, cell_h := term_cell_pixels()
	out_w := cols * cell_w
	out_h := rows * cell_h
	out_w, out_h = fit_disp(vs.src_w, vs.src_h, cols, rows, cell_w, cell_h, full_cli)

	vp := new(VideoPlayer)
	vp.vs = vs
	vp.out_w = out_w
	vp.out_h = out_h

	color_idx := 4
	if color_cli != 0 {
		for lv, li in PALETTE_LEVELS {
			if lv == color_cli {
				color_idx = li
			}
		}
	}
	pal := get_palette(PALETTE_LEVELS[color_idx])

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
	if start_sec > 0.0 {
		abs_t := start_sec
		if vs.duration_sec > 0 {
			abs_t = clamp(start_sec, 0.0, max(vs.duration_sec - 0.5, 0.0))
		}
		playback_start = time.time_add(time.now(), time.Duration(-abs_t * 1000000000.0))
		if has_audio {
			audio_seek(i64(abs_t * 1000000000.0), i64(abs_t * 1000000000.0))
		}
		video_request_seek(vp, i64(abs_t * 1000000000.0))
	}
	pause_start := start_wall
	paused := false
	muted := false
	pause_clock := 0.0
	volume := 1.0
	if vol_cli >= 0.0 {
		volume = vol_cli / 100.0
	}
	if has_audio {
		audio_set_volume(f32(volume))
	}
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
	full_disp := full_cli
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
		if sync.atomic_load(&vp.stop) != 0 {
			quit = true
			continue
		}
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
					video_request_seek(vp, i64(pause_clock * 1000000000.0))
				}
			case KEY_VOL_UP:
				volume = min(volume + 0.05, 1.0)
				muted = false
				audio_set_volume(f32(volume))
			case KEY_VOL_DOWN:
				volume = max(volume - 0.05, 0.0)
				muted = false
				audio_set_volume(f32(volume))
			case KEY_SEEK_FWD, KEY_SEEK_BACK:
				cur := time.duration_seconds(time.since(playback_start))
				if paused {
					cur = pause_clock
				}
				target := cur + 5.0
				if k == KEY_SEEK_BACK {
					target = cur - 5.0
				}
				loff := f64(sync.atomic_load(&vp.loop_offset)) / 1000000000.0
				media_t := clamp(target - loff, 0.0, max(vs.duration_sec - 0.5, 0.0))
				abs_t := media_t + loff
				playback_start = time.time_add(time.now(), time.Duration(-abs_t * 1000000000.0))
				if paused {
					pause_clock = abs_t
					pause_start = time.now()
				}
				if has_audio {
					audio_seek(i64(media_t * 1000000000.0), i64(abs_t * 1000000000.0))
				}
				if have_pending {
					video_pool_put(vp, pending.buf)
					have_pending = false
				}
				video_request_seek(vp, i64(abs_t * 1000000000.0))
				force_frame = true
			case KEY_MUTE:
				muted = !muted
				if muted {
					audio_set_volume(0.0)
				} else {
					audio_set_volume(f32(volume))
				}
			case KEY_COLOR_UP:
				color_idx = min(color_idx + 1, len(PALETTE_LEVELS) - 1)
				pal = get_palette(PALETTE_LEVELS[color_idx])
				if paused {
					rerender_freeze(&rc, out_w, out_h, start_col, start_row, pal)
				}
			case KEY_COLOR_DOWN:
				color_idx = max(color_idx - 1, 0)
				pal = get_palette(PALETTE_LEVELS[color_idx])
				if paused {
					rerender_freeze(&rc, out_w, out_h, start_col, start_row, pal)
				}
			case KEY_SIZE:
				full_disp = !full_disp
				sync.atomic_store(&winch_flag, 1)
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
			nw, nh := fit_disp(vs.src_w, vs.src_h, cols, rows, cell_w, cell_h, full_disp)
			if nw != out_w || nh != out_h {
				if have_pending {
					video_pool_put(vp, pending.buf)
					have_pending = false
				}
				out_w, out_h = nw, nh
				start_col = max((cols - (out_w + cell_w - 1) / cell_w) / 2 + 1, 1)
				start_row = max((rows - (out_h + cell_h - 1) / cell_h) / 2 + 1, 1)
				video_request_resize(vp, out_w, out_h)
				render_submit(&rc, RenderJob{clear = true})
				if paused {
					rerender_freeze_scaled(&rc, out_w, out_h, start_col, start_row, pal)
				} else {
					force_frame = true
				}
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
				fps_row := start_row + (out_h + cell_h - 1) / cell_h + 1
				pos := pause_clock
				if !paused {
					pos = time.duration_seconds(time.since(playback_start))
				}
				loff := f64(sync.atomic_load(&vp.loop_offset)) / 1000000000.0
				media_pos := clamp(pos - loff, 0.0, vs.duration_sec)
				ptext := fmt.tprintf("%d:%02d/%d:%02d",
					int(media_pos) / 60, int(media_pos) %% 60,
					int(vs.duration_sec) / 60, int(vs.duration_sec) %% 60)
				seg := fmt.tprintf("%d%%", int(volume * 100 + 0.5))
				if muted {
					seg = "MUTE"
				}
				size_mark := "\u2922"
				if !full_disp {
					size_mark = "\u26F6"
				}
				label := fmt.tprintf("[ %d | %.3f/%sfps | %s | %s | %s ]", pal.nc, inst, fps_preset(src_fps), ptext, seg, size_mark)
				sync.mutex_lock(&rc.stdout_mu)
				if fps_row <= rows {
					fps_ui_show(&fps_ui, fps_row, cols, label)
				} else {
					fps_ui_hide(&fps_ui)
				}
				sync.mutex_unlock(&rc.stdout_mu)
			}
		}

		if paused && !force_frame {
			time.sleep(2 * time.Millisecond)
			continue
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
		if pts > clock + 5.0 {
			video_pool_put(vp, pending.buf)
			have_pending = false
			continue
		}
		if pts > clock + 0.002 && !force_frame {
			remain := min(pts - clock - 0.001, 0.05)
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

		force_frame = false
		start_col = max((cols - (out_w + cell_w - 1) / cell_w) / 2 + 1, 1)
		start_row = max((rows - (out_h + cell_h - 1) / cell_h) / 2 + 1, 1)
		render_submit(&rc, RenderJob{
			buf = pending.buf,
			w = out_w,
			h = out_h,
			start_col = start_col,
			start_row = start_row,
			pal = pal,
		})
		have_pending = false
	}

	render_stop(&rc)
	if len(rc.freeze) > 0 {
		video_pool_put(vp, rc.freeze)
		rc.freeze = nil
	}
	video_stop(vp)
	video_close(vp)
	audio_shutdown()
	for i in 0..<len(rc.bufs) {
		if len(rc.bufs[i].buf) > 0 {
			free(&rc.bufs[i].buf[0])
		}
	}
	term_leave_raw()
	fmt.print("\x1b[?1049l\x1b[?25h\x1b[?1000h\x1b[?1002h\x1b[?1003h")
	if profile_cli {
		prof_report()
	}
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
		spaces := "                                                                                                "
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
	ui.prev_len = text_width(text)
}

// fps_ui_show erases the previous label and draws bold text centered on row.
fps_ui_show :: proc(ui: ^FpsUI, row, cols: int, text: string) {
	fps_ui_show_at(ui, row, max((cols - text_width(text)) / 2 + 1, 1), text)
}

// fit_disp returns the video size fitted to the display area. When full is
// false the area is halved in both dimensions.
fit_disp :: proc(src_w, src_h, cols, rows, cell_w, cell_h: int, full: bool) -> (int, int) {
	w := cols * cell_w
	h := rows * cell_h
	if !full {
		w /= 2
		h /= 2
	}
	return fit_aspect(src_w, src_h, w, h)
}

// text_width returns the printable width of s counting codepoints.
text_width :: proc(s: string) -> int {
	n := 0
	for i in 0..<len(s) {
		if s[i] & 0xC0 != 0x80 {
			n += 1
		}
	}
	return n
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
