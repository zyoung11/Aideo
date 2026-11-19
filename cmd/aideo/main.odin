package main

import "core:c"
import aideo "../.."
import "core:fmt"
import "core:os"
import "core:strconv"
import "core:time"
import "core:sync"
import "core:thread"

main :: proc() {
	if len(os.args) >= 2 && os.args[1] == "citest" { aideo.ci_test(); return }
	if len(os.args) >= 2 && os.args[1] == "cisws" { aideo.ci_sws(); return }
	if len(os.args) >= 2 && os.args[1] == "ciframe" { aideo.ci_frame(); return }
	if len(os.args) >= 2 && os.args[1] == "cibench" { aideo.ci_bench(); return }
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
		for lv in aideo.PALETTE_LEVELS {
			if lv == color_cli {
				found = true
			}
		}
		if !found {
			cli_error("color count must be one of 2 8 16 32 64 128 256")
			os.exit(1)
		}
	}
	path := aideo.strings_clone_to_cstring(file_arg)
	in_tmux := os.get_env("TMUX", context.allocator) != ""

	aideo.av_log_set_level(aideo.AV_LOG_QUIET)

	vs := aideo.video_open(path)
	if vs == nil {
		fmt.eprintfln("error: cannot open video: %s", file_arg)
		os.exit(1)
	}
	if aideo.is_image_codec_name(vs.codec_name) {
		if aideo.stream_is_animated(vs, file_arg) {
			vs.loop_forever = true
		} else {
			aideo.video_close_stream(vs)
			aideo.photo_run(path, file_arg, full_cli)
			return
		}
	} else if aideo.is_animation_codec_name(vs.codec_name) {
		vs.loop_forever = true
	}

	cols, rows := aideo.term_size()
	cell_w, cell_h := aideo.term_cell_pixels()
	out_w := cols * cell_w
	out_h := rows * cell_h
	out_w, out_h = aideo.fit_disp(vs.src_w, vs.src_h, cols, rows, cell_w, cell_h, full_cli)

	vp := new(aideo.VideoPlayer)
	vp.vs = vs
	vp.out_w = out_w
	vp.out_h = out_h

	color_idx := 4
	if color_cli != 0 {
		for lv, li in aideo.PALETTE_LEVELS {
			if lv == color_cli {
				color_idx = li
			}
		}
	}
	pal := aideo.get_palette(aideo.PALETTE_LEVELS[color_idx])

	fmt.print("\x1b[?1049h\x1b[?25l\x1b[?1000l\x1b[?1002l\x1b[?1003l")
	if in_tmux {
		fmt.print("\x1b[?1004h")
	}
	fmt.print("\x1b[2J\x1b[3J\x1b[H")

	has_audio := aideo.audio_init()
	if has_audio {
		has_audio = aideo.audio_start(path)
	}

	aideo.term_enter_raw()
	aideo.term_watch_winch()
	aideo.key_start()
	aideo.video_start(vp)

	start_wall := time.now()
	playback_start := start_wall
	if start_sec > 0.0 {
		abs_t := start_sec
		if vs.duration_sec > 0 {
			abs_t = clamp(start_sec, 0.0, max(vs.duration_sec - 0.5, 0.0))
		}
		playback_start = time.time_add(time.now(), time.Duration(-abs_t * 1000000000.0))
		if has_audio {
			aideo.audio_seek(i64(abs_t * 1000000000.0), i64(abs_t * 1000000000.0))
		}
		aideo.video_request_seek(vp, i64(abs_t * 1000000000.0))
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
		aideo.audio_set_volume(f32(volume))
	}
	pool: aideo.StripPool
	aideo.strip_pool_init(&pool, max(aideo.cpu_count() / 2, 4))
	defer aideo.strip_pool_stop(&pool)
	rc: aideo.RenderCtx
	rc.vp = vp
	rc.pool = &pool
	for i in 0..<len(rc.bufs) {
		aideo.buffer_init(&rc.bufs[i], out_w * out_h / 2)
	}
	aideo.render_start(&rc)
	pending: aideo.FrameMsg
	have_pending := false
	force_frame := false
	start_col := 1
	start_row := 1
	show_fps := false
	full_disp := full_cli
	fps_ui: aideo.FpsUI
	full_t := time.now()
	src_fps := vs.fps
	fps_sample_t := time.now()
	fps_sample_n := 0
	fps_ui_t := time.Time{}

	quit := false
	_last_frame_t := time.now()
	for !quit {
		_loop_t := time.now()
		aideo.prof_add(aideo.PROF_FRAME_GAP, i64(time.since(_last_frame_t)))
		_last_frame_t = _loop_t
		if sync.atomic_load(&vp.stop) != 0 {
			quit = true
			continue
		}
		for {
			k, ok := aideo.key_pop()
			if !ok {
				break
			}
			switch k {
			case aideo.KEY_QUIT:
				quit = true
			case aideo.KEY_PAUSE:
				paused = !paused
				aideo.audio_set_paused(paused)
				if paused {
					pause_start = time.now()
					pause_clock = time.duration_seconds(time.since(playback_start))
				} else {
					playback_start = time.time_add(playback_start, time.since(pause_start))
					aideo.video_request_seek(vp, i64(pause_clock * 1000000000.0))
				}
			case aideo.KEY_VOL_UP:
				volume = min(volume + 0.05, 1.0)
				muted = false
				aideo.audio_set_volume(f32(volume))
			case aideo.KEY_VOL_DOWN:
				volume = max(volume - 0.05, 0.0)
				muted = false
				aideo.audio_set_volume(f32(volume))
			case aideo.KEY_SEEK_FWD, aideo.KEY_SEEK_BACK:
				cur := time.duration_seconds(time.since(playback_start))
				if paused {
					cur = pause_clock
				}
				target := cur + 5.0
				if k == aideo.KEY_SEEK_BACK {
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
					aideo.audio_seek(i64(media_t * 1000000000.0), i64(abs_t * 1000000000.0))
				}
				if have_pending {
					aideo.video_pool_put(vp, pending.buf)
					have_pending = false
				}
				aideo.video_request_seek(vp, i64(abs_t * 1000000000.0))
				force_frame = true
			case aideo.KEY_MUTE:
				muted = !muted
				if muted {
					aideo.audio_set_volume(0.0)
				} else {
					aideo.audio_set_volume(f32(volume))
				}
			case aideo.KEY_COLOR_UP:
				color_idx = min(color_idx + 1, len(aideo.PALETTE_LEVELS) - 1)
				pal = aideo.get_palette(aideo.PALETTE_LEVELS[color_idx])
				if paused {
					aideo.rerender_freeze(&rc, out_w, out_h, start_col, start_row, pal)
				}
			case aideo.KEY_COLOR_DOWN:
				color_idx = max(color_idx - 1, 0)
				pal = aideo.get_palette(aideo.PALETTE_LEVELS[color_idx])
				if paused {
					aideo.rerender_freeze(&rc, out_w, out_h, start_col, start_row, pal)
				}
			case aideo.KEY_SIZE:
				full_disp = !full_disp
				sync.atomic_store(&aideo.winch_flag, 1)
			case aideo.KEY_FOCUS_IN:
				if in_tmux {
					aideo.render_submit(&rc, aideo.RenderJob{clear = true})
					if paused {
						aideo.rerender_freeze_scaled(&rc, out_w, out_h, start_col, start_row, pal)
					} else {
						force_frame = true
					}
				}
			case aideo.KEY_FPS:
				show_fps = !show_fps
				fps_ui_t = time.Time{}
				if !show_fps {
					sync.mutex_lock(&rc.stdout_mu)
					aideo.fps_ui_hide(&fps_ui)
					sync.mutex_unlock(&rc.stdout_mu)
				}
			}
		}

		if aideo.term_take_winch() {
			cols, rows = aideo.term_size()
			cell_w, cell_h = aideo.term_cell_pixels()
			nw, nh := aideo.fit_disp(vs.src_w, vs.src_h, cols, rows, cell_w, cell_h, full_disp)
			if have_pending {
				aideo.video_pool_put(vp, pending.buf)
				have_pending = false
			}
			if nw != out_w || nh != out_h {
				out_w, out_h = nw, nh
				start_col = max((cols - (out_w + cell_w - 1) / cell_w) / 2 + 1, 1)
				start_row = max((rows - (out_h + cell_h - 1) / cell_h) / 2 + 1, 1)
				aideo.video_request_resize(vp, out_w, out_h)
				fps_ui.prev_len = 0
			}
			aideo.render_submit(&rc, aideo.RenderJob{clear = true})
			if paused {
				aideo.rerender_freeze_scaled(&rc, out_w, out_h, start_col, start_row, pal)
			} else {
				force_frame = true
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
				label := fmt.tprintf("[ %d | %.3f/%sfps | %s | %s | %s ]", pal.nc, inst, aideo.fps_preset(src_fps), ptext, seg, size_mark)
				sync.mutex_lock(&rc.stdout_mu)
				if fps_row <= rows {
					aideo.fps_ui_show(&fps_ui, fps_row, cols, label)
				} else {
					aideo.fps_ui_hide(&fps_ui)
				}
				sync.mutex_unlock(&rc.stdout_mu)
			}
		}

		if paused && !force_frame {
			time.sleep(2 * time.Millisecond)
			continue
		}

		if !have_pending {
			m, ok := aideo.fq_try_pop(&vp.queue)
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
			aideo.video_pool_put(vp, pending.buf)
			have_pending = false
			continue
		}

		pts := pending.pts_sec
		clock := pause_clock
		if !paused {
			clock = time.duration_seconds(time.since(playback_start))
			if has_audio {
				drift := clock - aideo.audio_clock_sec()
				if drift > 0.05 || drift < -0.05 {
					playback_start = time.time_add(time.now(), time.Duration(-aideo.audio_clock_sec() * 1e9))
					clock = time.duration_seconds(time.since(playback_start))
				}
			}
		}
		lag_tol := 0.100
		if src_fps > 0 {
			lag_tol = 2.0 / src_fps
		}
		if pts > clock + 5.0 {
			aideo.video_pool_put(vp, pending.buf)
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
			aideo.video_pool_put(vp, pending.buf)
			have_pending = false
			continue
		}

		force_frame = false
		start_col = max((cols - (out_w + cell_w - 1) / cell_w) / 2 + 1, 1)
		start_row = max((rows - (out_h + cell_h - 1) / cell_h) / 2 + 1, 1)
		full_now := in_tmux && time.duration_seconds(time.since(full_t)) >= 2.0
		if full_now {
			full_t = time.now()
		}
		aideo.render_submit(&rc, aideo.RenderJob{
			buf = pending.buf,
			w = out_w,
			h = out_h,
			start_col = start_col,
			start_row = start_row,
			pal = pal,
			reset_cache = full_now,
		})
		have_pending = false
	}

	aideo.render_stop(&rc)
	if len(rc.freeze) > 0 {
		aideo.video_pool_put(vp, rc.freeze)
		rc.freeze = nil
	}
	aideo.video_stop(vp)
	aideo.video_close(vp)
	aideo.audio_shutdown()
	for i in 0..<len(rc.bufs) {
		if len(rc.bufs[i].buf) > 0 {
			free(&rc.bufs[i].buf[0])
		}
	}
	aideo.term_leave_raw()
	fmt.print("\x1b[?1049l\x1b[?25h\x1b[?1000h\x1b[?1002h\x1b[?1003h")
	if in_tmux {
		fmt.print("\x1b[?1004l")
	}
	if profile_cli {
		aideo.prof_report()
	}
}
