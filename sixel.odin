package aideo

import "core:c"
import "core:sync"
import "core:time"
import "core:thread"
import x86 "core:simd/x86"

foreign import avx2lib { "avx2_quant.o" }

foreign avx2lib {
	quant_row_avx2 :: proc(dst: [^]u8, rgba: [^]u8, w: c.int, dpos_r: [^]u8, dneg_r: [^]u8, dpos_g: [^]u8, dneg_g: [^]u8, dpos_b: [^]u8, dneg_b: [^]u8, r_shr: c.int, g_shr: c.int, b_shr: c.int, r_shift: c.int, g_shift: c.int) ---
	hash_strip_avx2 :: proc(rgba: [^]u8, width: c.int, y_start: c.int, y_end: c.int) -> u64 ---
}

BAYER_4X4 := [16]i8{0, 8, 2, 10, 12, 4, 14, 6, 3, 11, 1, 9, 15, 7, 13, 5}

PALETTE_LEVELS := [7]int{2, 8, 16, 32, 64, 128, 256}
PALETTE_BITS  := [7][3]int{{0, 0, 0}, {1, 1, 1}, {2, 1, 1}, {2, 2, 1}, {2, 2, 2}, {3, 2, 2}, {3, 3, 2}}

DitherSet :: struct {
	dpos_r, dneg_r: [32]u8,
	dpos_g, dneg_g: [32]u8,
	dpos_b, dneg_b: [32]u8,
}

SixelPalette :: struct {
	nc:      int,
	is_gray: bool,
	r_bits, g_bits, b_bits: int,
	r_shift, g_shift:      int,
	colors:  [256]u32,
	lut_r:   [16][256]u8,
	lut_g:   [16][256]u8,
	lut_b:   [16][256]u8,
	dither:  [16]i8,
	dither_cache:  [16]DitherSet,
	dither_ready:  [16]bool,
}

StripCache :: struct {
	hashes:  []u64,
	rle:     [][dynamic]u8,
	pix:     [][dynamic]u8,
	changed: []u8,
}

// strip_cache_init allocates per-strip cache storage for total_strips bands.
strip_cache_init :: proc(cache: ^StripCache, total_strips: int) {
	cache.hashes = make([]u64, total_strips)
	cache.rle = make([][dynamic]u8, total_strips)
	cache.pix = make([][dynamic]u8, total_strips)
	cache.changed = make([]u8, total_strips)
}

make_dither :: proc(levels: int) -> [16]i8 {
	step := 256 / (levels - 1)
	d: [16]i8
	for v, i in BAYER_4X4 {
		d[i] = i8((int(v) - 8) * step / 16)
	}
	return d
}

retro_levels :: proc(dst: []u8, levels: int) {
	switch levels {
	case 2:
		dst[0], dst[1] = 20, 200
	case 4:
		dst[0], dst[1], dst[2], dst[3] = 0, 105, 210, 255
	case:
		for i in 0..<levels {
			dst[i] = u8(i * 255 / (levels - 1))
		}
	}
}

retro_blue_levels :: proc(dst: []u8, levels: int) {
	switch levels {
	case 2:
		dst[0], dst[1] = 10, 150
	case:
		for i in 0..<levels {
			dst[i] = u8(i * 255 / (levels - 1))
		}
	}
}

build_luts :: proc(p: ^SixelPalette) {
	dR := make_dither(1 << u32(p.r_bits))
	dG := make_dither(1 << u32(p.g_bits))
	dB := make_dither(1 << u32(p.b_bits))
	for b in 0..<16 {
		for v in 0..<256 {
			s := v + int(dR[b])
			s = clamp(s, 0, 255)
			p.lut_r[b][v] = u8(s >> u32(8 - p.r_bits)) << u32(p.r_shift)

			s = v + int(dG[b])
			s = clamp(s, 0, 255)
			p.lut_g[b][v] = u8(s >> u32(8 - p.g_bits)) << u32(p.g_shift)

			s = v + int(dB[b])
			s = clamp(s, 0, 255)
			p.lut_b[b][v] = u8(s >> u32(8 - p.b_bits))
		}
	}
}

build_uniform_palette :: proc(p: ^SixelPalette, r_bits, g_bits, b_bits: int) {
	r_levels := 1 << u32(r_bits)
	g_levels := 1 << u32(g_bits)
	b_levels := 1 << u32(b_bits)
	p.nc = r_levels * g_levels * b_levels
	p.r_bits, p.g_bits, p.b_bits = r_bits, g_bits, b_bits
	p.g_shift = b_bits
	p.r_shift = g_bits + b_bits
	for ri in 0..<r_levels {
		for gi in 0..<g_levels {
			for bi in 0..<b_levels {
				idx := ri << u32(p.r_shift) | gi << u32(p.g_shift) | bi
				r := u8(ri * 255 / (r_levels - 1))
				g := u8(gi * 255 / (g_levels - 1))
				b := u8(bi * 255 / (b_levels - 1))
				p.colors[idx] = u32(r) << 16 | u32(g) << 8 | u32(b)
			}
		}
	}
	build_luts(p)
}

build_retro_palette :: proc(p: ^SixelPalette, r_bits, g_bits, b_bits: int) {
	r_levels := 1 << u32(r_bits)
	g_levels := 1 << u32(g_bits)
	b_levels := 1 << u32(b_bits)
	p.nc = r_levels * g_levels * b_levels
	p.r_bits, p.g_bits, p.b_bits = r_bits, g_bits, b_bits
	p.g_shift = b_bits
	p.r_shift = g_bits + b_bits
	r_lv: [4]u8
	g_lv: [4]u8
	b_lv: [4]u8
	retro_levels(r_lv[:], r_levels)
	retro_levels(g_lv[:], g_levels)
	retro_blue_levels(b_lv[:], b_levels)
	for ri in 0..<r_levels {
		for gi in 0..<g_levels {
			for bi in 0..<b_levels {
				idx := ri << u32(p.r_shift) | gi << u32(p.g_shift) | bi
				p.colors[idx] = u32(r_lv[ri]) << 16 | u32(g_lv[gi]) << 8 | u32(b_lv[bi])
			}
		}
	}
	build_luts(p)
}

build_gray_palette :: proc(p: ^SixelPalette, levels: int) {
	p.nc = levels
	p.is_gray = true
	for i in 0..<levels {
		v := u8(i * 255 / (levels - 1))
		p.colors[i] = u32(v) << 16 | u32(v) << 8 | u32(v)
	}
	step := 256 / levels
	for b in 0..<16 {
		p.dither[b] = i8((int(BAYER_4X4[b]) - 8) * step / 16)
	}
}

get_palette :: proc(nc: int) -> ^SixelPalette {
	if nc == 2 {
		p := new(SixelPalette)
		build_gray_palette(p, 2)
		return p
	}
	for lvl, i in PALETTE_LEVELS {
		if lvl == nc {
			bits := PALETTE_BITS[i]
			p := new(SixelPalette)
			if nc == 16 || nc == 32 {
				build_retro_palette(p, bits[0], bits[1], bits[2])
			} else {
				build_uniform_palette(p, bits[0], bits[1], bits[2])
			}
			return p
		}
	}
	return nil
}

// strip_pixel_hash_rgba is the scalar reference for hash_strip_avx2. It
// hashes the band as one little-endian u32 stream with a Horner style
// polynomial over eight lanes and two multiplier sets.
strip_pixel_hash_rgba :: proc(data: []u8, width, y_start, y_end: int) -> u64 {
	P1 :: u32(0x9E3779B1)
	P2 :: u32(0x85EBCA77)
	P3 :: u32(0xC2B2AE3D)
	P4 :: u32(0x27D4EB2F)
	a1: [8]u32
	a2: [8]u32
	for k in 0..<8 {
		a1[k] = 0x9E3779B9
		a2[k] = 0x85EBCA6B
	}
	base := y_start * width * 4
	nwords := (y_end - y_start) * width
	for i in 0..<nwords {
		q := base + i * 4
		v := u32(data[q]) | u32(data[q+1]) << 8 | u32(data[q+2]) << 16 | u32(data[q+3]) << 24
		k := i % 8
		a1[k] = a1[k] * P1 + v * P2
		a2[k] = a2[k] * P3 + v * P4
	}
	s1: u32
	s2: u32
	for k in 0..<8 {
		s1 += a1[k] * u32(2 * k + 1)
		s2 += a2[k] * u32(2 * k + 1)
	}
	return u64(s1) << 32 | u64(s2)
}

@(enable_target_feature="sse2,ssse3") shuf_mask_r :: proc "contextless" () -> x86.__m128i {
	return x86._mm_set_epi8(-1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, 12, 8, 4, 0)
}

@(enable_target_feature="sse2,ssse3") shuf_mask_g :: proc "contextless" () -> x86.__m128i {
	return x86._mm_set_epi8(-1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, 13, 9, 5, 1)
}

@(enable_target_feature="sse2,ssse3") shuf_mask_b :: proc "contextless" () -> x86.__m128i {
	return x86._mm_set_epi8(-1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, 14, 10, 6, 2)
}

@(private)
quantize_row_uniform_simd :: proc(dst: []u8, rgba: []u8, w, yb4: int, p: ^SixelPalette) {
	dR := make_dither(1 << u32(p.r_bits))
	dG := make_dither(1 << u32(p.g_bits))
	dB := make_dither(1 << u32(p.b_bits))
	dpos_r: [32]u8
	dneg_r: [32]u8
	dpos_g: [32]u8
	dneg_g: [32]u8
	dpos_b: [32]u8
	dneg_b: [32]u8
	for i in 0..<4 {
		col := yb4 | i
		dr := int(dR[col])
		dg := int(dG[col])
		db := int(dB[col])
		if dr >= 0 {
			dpos_r[i] = u8(dr)
			dpos_r[16 + i] = u8(dr)
		} else {
			dneg_r[i] = u8(-dr)
			dneg_r[16 + i] = u8(-dr)
		}
		if dg >= 0 {
			dpos_g[i] = u8(dg)
			dpos_g[16 + i] = u8(dg)
		} else {
			dneg_g[i] = u8(-dg)
			dneg_g[16 + i] = u8(-dg)
		}
		if db >= 0 {
			dpos_b[i] = u8(db)
			dpos_b[16 + i] = u8(db)
		} else {
			dneg_b[i] = u8(-db)
			dneg_b[16 + i] = u8(-db)
		}
	}
	quant_row_avx2(&dst[0], &rgba[0], c.int(w), &dpos_r[0], &dneg_r[0], &dpos_g[0], &dneg_g[0], &dpos_b[0], &dneg_b[0],
		c.int(8 - p.r_bits), c.int(8 - p.g_bits), c.int(8 - p.b_bits),
		c.int(p.r_shift), c.int(p.g_shift))
}

@(private)
quantize_row_uniform_scalar :: proc(dst: []u8, rgba: []u8, w, yb4, x_start: int, p: ^SixelPalette) {
	for x in x_start..<w {
		b := yb4 | (x & 3)
		pi := x * 4
		ci := int(p.lut_r[b][rgba[pi]]) | int(p.lut_g[b][rgba[pi+1]]) | int(p.lut_b[b][rgba[pi+2]])
		dst[x] = u8(ci)
	}
}

@(private)
quantize_row_gray :: proc(dst: []u8, rgba: []u8, w, yb4: int, p: ^SixelPalette) {
	shift := 8
	for n := p.nc >> 1; n > 0; n >>= 1 {
		shift -= 1
	}
	for x in 0..<w {
		pi := x * 4
		gray := (6966 * int(rgba[pi]) + 23436 * int(rgba[pi+1]) + 2366 * int(rgba[pi+2])) >> 15
		gray = clamp(gray + int(p.dither[yb4 | (x & 3)]), 0, 255)
		dst[x] = u8(gray >> u32(shift))
	}
}

Buffer :: struct {
	buf: []u8,
	pos: int,
}

buffer_init :: proc(b: ^Buffer, cap: int) {
	b.buf = make([]u8, max(cap, 4096))
	b.pos = 0
}

buffer_reset :: proc(b: ^Buffer) {
	b.pos = 0
}

buffer_grow :: proc(b: ^Buffer, need: int) {
	new_cap := max(len(b.buf) * 2, b.pos + need)
	nb := make([]u8, new_cap)
	copy(nb, b.buf[:b.pos])
	free(&b.buf[0])
	b.buf = nb
}

write_byte :: proc (b: ^Buffer, v: u8) {
	if b.pos >= len(b.buf) {
		buffer_grow(b, 1)
	}
	b.buf[b.pos] = v
	b.pos += 1
}

write_bytes :: proc(b: ^Buffer, data: []u8) {
	if b.pos + len(data) > len(b.buf) {
		buffer_grow(b, len(data))
	}
	copy(b.buf[b.pos:], data)
	b.pos += len(data)
}

buffer_mark :: proc(b: ^Buffer) -> int {
	return b.pos
}

StripState :: struct {
	buf:        []u8,
	seen:       []u16,
	dirty:      [dynamic]int,
	min_x:      [256]i32,
	max_x:      [256]i32,
	epoch:      u16,
	pixel_hash: u64,
	prev_width: int,
	prev_nc:    int,
}

strip_state_reset :: proc(st: ^StripState, width, nc: int) {
	strip_cap := nc * width
	if len(st.buf) < strip_cap {
		st.buf = make([]u8, strip_cap)
	}
	if len(st.seen) < nc {
		st.seen = make([]u16, nc)
	}
	st.epoch += 1
	if st.epoch == 0 {
		for i in 0..<len(st.seen) {
			st.seen[i] = 0
		}
		st.epoch = 1
	}
	for i in 0..<256 {
		st.min_x[i] = 1 << 30
		st.max_x[i] = -1
	}
	if st.prev_width == width && st.prev_nc == nc {
		for c in st.dirty {
			base := c * width
			for i in base..<base + width {
				st.buf[i] = 0
			}
		}
	} else {
		for i in 0..<strip_cap {
			st.buf[i] = 0
		}
	}
	st.prev_width = width
	st.prev_nc = nc
	clear(&st.dirty)
}

emit_run :: proc(out: ^Buffer, ch: u8, count_in: int) {
	count := count_in
	for count > 0 {
		c := count
		if c > 255 {
			c = 255
		}
		if c >= 4 {
			write_byte(out, 0x21)
			write_sixel_num(out, c)
			write_byte(out, ch + 63)
		} else {
			for i in 0..<c {
				write_byte(out, ch + 63)
			}
		}
		count -= c
	}
}

write_sixel_num :: proc(b: ^Buffer, n: int) {
	if n >= 10000 {
		write_byte(b, u8(0x30 + n/10000))
	}
	if n >= 1000 {
		write_byte(b, u8(0x30 + (n/1000)%10))
	}
	if n >= 100 {
		write_byte(b, u8(0x30 + (n/100)%10))
	}
	if n >= 10 {
		write_byte(b, u8(0x30 + (n/10)%10))
	}
	write_byte(b, u8(0x30 + n%10))
}

encode_strip :: proc(out: ^Buffer, st: ^StripState, width: int) {
	for c in st.dirty {
		base := c * width
		row := st.buf[base:base+width]
		x0 := int(st.min_x[c])
		x1 := int(st.max_x[c])

		write_byte(out, 0x24)
		write_byte(out, 0x23)
		write_sixel_num(out, c + 1)

		if x0 > 0 {
			emit_run(out, 0, x0)
		}

		last_ch: u8 = row[x0]
		run_count := 0
		x := x0
		spread := u64(last_ch) * 0x0101010101010101
		for x <= x1 {
			ch := row[x]
			if ch == last_ch {
				run_count += 1
				x += 1
				for run_count <= 247 && x + 8 <= x1 + 1 {
					v := (^u64)(&row[x])^
					if v != spread {
						break
					}
					run_count += 8
					x += 8
				}
				if run_count > 255 {
					emit_run(out, last_ch, 255)
					run_count -= 255
				}
				continue
			}
			emit_run(out, last_ch, run_count)
			last_ch = ch
			spread = u64(last_ch) * 0x0101010101010101
			run_count = 1
			x += 1
		}
		emit_run(out, last_ch, run_count)
	}
}

StripWorkerCtx :: struct {
	rgba:        []u8,
	width:       int,
	height:      int,
	pal:         ^SixelPalette,
	cache:       ^StripCache,
	next_job:    i64,
	total_strips: int,
}

StripPool :: struct {
	ctx:     StripWorkerCtx,
	start:   sync.Sema,
	done:    sync.Sema,
	exited:  sync.Sema,
	workers: []^thread.Thread,
	stopped: i32,
}

StripWorkerState :: struct {
	st:     StripState,
	row_ci: []u8,
	cur:    []u8,
	buf:    Buffer,
}

// strip_pool_run encodes every unencoded band of one frame.
strip_pool_run :: proc(ctx: ^StripWorkerCtx, ws: ^StripWorkerState) {
	st := &ws.st
	if len(ws.row_ci) < ctx.width {
		ws.row_ci = make([]u8, ctx.width)
	}
	if len(ws.buf.buf) < ctx.width * 64 {
		if len(ws.buf.buf) > 0 {
			free(&ws.buf.buf[0])
		}
		buffer_init(&ws.buf, ctx.width * 64)
	}
	row_ci := ws.row_ci
	buf := &ws.buf

	pal := ctx.pal
	nc := pal.nc
	width := ctx.width
	uniform := !pal.is_gray && nc != 16 && nc != 32

	for {
		srow := int(sync.atomic_add(&ctx.next_job, 1))
		if srow >= ctx.total_strips {
			return
		}
		y_start := srow * 6
		y_end := min(y_start + 6, ctx.height)
		n_rows := y_end - y_start
		_t0 := time.now()
		p_hash := hash_strip_avx2(&ctx.rgba[0], c.int(width), c.int(y_start), c.int(y_end))
		prof_add(PROF_ENCODE_HASH, i64(time.since(_t0)))

		if ctx.cache.hashes[srow] == p_hash && len(ctx.cache.rle[srow]) > 0 {
			continue
		}

		buffer_reset(buf)
		strip_state_reset(st, width, nc)
		_t1 := time.now()

		prev := ctx.cache.pix[srow]
		if len(ws.cur) < n_rows * width {
			ws.cur = make([]u8, n_rows * width)
		}
		row_offset := y_start * width * 4
		for dy in 0..<n_rows {
			yb4 := ((y_start + dy) & 3) << 2
			bit := u8(1 << u32(dy))
			row := ctx.rgba[row_offset:row_offset+width*4]
			if pal.is_gray {
				quantize_row_gray(row_ci, row, width, yb4, pal)
			} else if uniform {
				quantize_row_uniform_simd(row_ci, row, width, yb4, pal)
				q := width &~ 3
				quantize_row_uniform_scalar(row_ci, row, width, yb4, q, pal)
			} else {
				quantize_row_uniform_scalar(row_ci, row, width, yb4, 0, pal)
			}
			copy(ws.cur[dy * width:], row_ci[:width])
			base := dy * width
			for x in 0..<width {
				ci := int(row_ci[x])
				if base + x < len(prev) && prev[base + x] == u8(ci) {
					continue
				}
				if st.seen[ci] != st.epoch {
					st.seen[ci] = st.epoch
					append(&st.dirty, ci)
				}
				st.buf[ci*width+x] |= bit
				if i32(x) < st.min_x[ci] {
					st.min_x[ci] = i32(x)
				}
				if i32(x) > st.max_x[ci] {
					st.max_x[ci] = i32(x)
				}
			}
			row_offset += width * 4
		}

		prof_add(PROF_ENCODE_SCAT, i64(time.since(_t1)))
		_t2 := time.now()
		encode_strip(buf, st, width)
		prof_add(PROF_ENCODE_RLE, i64(time.since(_t2)))

		ctx.cache.hashes[srow] = p_hash
		clear(&ctx.cache.rle[srow])
		append(&ctx.cache.rle[srow], ..buf.buf[:buf.pos])
		ctx.cache.changed[srow] = 1
		clear(&ctx.cache.pix[srow])
		append(&ctx.cache.pix[srow], ..ws.cur[:n_rows * width])
	}
}

strip_pool_worker :: proc(data: rawptr) {
	pool := (^StripPool)(data)
	defer sync.sema_post(&pool.exited)
	ws: StripWorkerState
	for {
		sync.sema_wait(&pool.start)
		if sync.atomic_load(&pool.stopped) != 0 {
			return
		}
		strip_pool_run(&pool.ctx, &ws)
		sync.sema_post(&pool.done)
	}
}

// strip_pool_init spawns num permanent worker threads for frame encoding.
strip_pool_init :: proc(pool: ^StripPool, workers: int) {
	pool.workers = make([]^thread.Thread, workers)
	for i in 0..<workers {
		pool.workers[i] = thread.create_and_start_with_data(pool, strip_pool_worker, nil, .Normal, true)
	}
}

// strip_pool_stop shuts the permanent workers down and waits for them.
strip_pool_stop :: proc(pool: ^StripPool) {
	sync.atomic_store(&pool.stopped, 1)
	sync.sema_post(&pool.start, len(pool.workers))
	for _ in 0..<len(pool.workers) {
		sync.sema_wait(&pool.exited)
	}
}

sixel_encode :: proc(pool: ^StripPool, out: ^Buffer, rgba: []u8, width, height: int, pal: ^SixelPalette, cache: ^StripCache) {
	nc := pal.nc
	total_strips := (height + 5) / 6

	write_bytes(out, []u8{0x1b, 0x50, 0x30, 0x3b, 0x31, 0x3b, 0x38, 0x71, 0x22})
	write_bytes(out, []u8{0x31, 0x3b, 0x31, 0x3b})
	write_sixel_num(out, width)
	write_byte(out, ';')
	write_sixel_num(out, height)
	for i in 0..<nc {
		c := pal.colors[i]
		write_byte(out, '#')
		write_sixel_num(out, i + 1)
		write_byte(out, ';')
		write_byte(out, '2')
		write_byte(out, ';')
		write_sixel_num(out, int((c >> 16 & 0xff) * 100 / 255))
		write_byte(out, ';')
		write_sixel_num(out, int((c >> 8 & 0xff) * 100 / 255))
		write_byte(out, ';')
		write_sixel_num(out, int((c & 0xff) * 100 / 255))
	}

	for i in 0..<total_strips {
		cache.changed[i] = 0
	}

	n_workers := len(pool.workers)
	pool.ctx = StripWorkerCtx{
		rgba = rgba,
		width = width,
		height = height,
		pal = pal,
		cache = cache,
		total_strips = total_strips,
	}
	_t3 := time.now()
	sync.atomic_store(&pool.ctx.next_job, 0)
	sync.sema_post(&pool.start, n_workers)
	for _ in 0..<n_workers {
		sync.sema_wait(&pool.done)
	}

	for srow in 0..<total_strips {
		if srow > 0 {
			write_byte(out, 0x2d)
		}
		if cache.changed[srow] != 0 {
			write_bytes(out, cache.rle[srow][:])
		}
	}
	write_bytes(out, []u8{0x1b, 0x5c})
	prof_add(PROF_ENCODE_ASM, i64(time.since(_t3)))
}
