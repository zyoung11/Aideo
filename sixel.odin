package aideo

import "core:sync"
import "core:thread"
import x86 "core:simd/x86"

BAYER_4X4 := [16]i8{0, 8, 2, 10, 12, 4, 14, 6, 3, 11, 1, 9, 15, 7, 13, 5}

PALETTE_LEVELS := [7]int{2, 8, 16, 32, 64, 128, 256}
PALETTE_BITS  := [7][3]int{{0, 0, 0}, {1, 1, 1}, {2, 1, 1}, {2, 2, 1}, {2, 2, 2}, {3, 2, 2}, {3, 3, 2}}

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
}

StripCache :: struct {
	hashes: []u64,
	rle:    [][dynamic]u8,
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

strip_pixel_hash_rgba :: proc(data: []u8, width, y_start, y_end: int) -> u64 {
	h0: u64 = 14695981039346656037
	h1: u64 = 14695981039346656037
	h2: u64 = 14695981039346656037
	h3: u64 = 14695981039346656037
	row_len := width * 4
	for y in y_start..<y_end {
		base := y * row_len
		row := data[base:base+row_len]
		n := len(row)
		lim := n &~ 3
		for i := 0; i < lim; i += 4 {
			h0 = (h0 ~ u64(row[i])) * 1099511628211
			h1 = (h1 ~ u64(row[i+1])) * 1099511628211
			h2 = (h2 ~ u64(row[i+2])) * 1099511628211
			h3 = (h3 ~ u64(row[i+3])) * 1099511628211
		}
		for i := lim; i < n; i += 1 {
			h0 = (h0 ~ u64(row[i])) * 1099511628211
		}
	}
	return h0 * 31 + h1 * 7 + h2 * 3 + h3
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
@(enable_target_feature="sse2,ssse3") quantize_row_uniform_simd :: proc(dst: []u8, rgba: []u8, w, yb4: int, p: ^SixelPalette) {
	zero := x86._mm_setzero_si128()

	step_r := 256 / ((1 << u32(p.r_bits)) - 1)
	step_g := 256 / ((1 << u32(p.g_bits)) - 1)
	step_b := 256 / ((1 << u32(p.b_bits)) - 1)

	d_r := [4]i8{0, 0, 0, 0}
	d_g := [4]i8{0, 0, 0, 0}
	d_b := [4]i8{0, 0, 0, 0}
	for i in 0..<4 {
		base := int(BAYER_4X4[yb4|i]) - 8
		d_r[i] = i8(base * step_r / 16)
		d_g[i] = i8(base * step_g / 16)
		d_b[i] = i8(base * step_b / 16)
	}

	dpos_r := x86._mm_set_epi8(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		i8(max(int(d_r[3]), 0)), i8(max(int(d_r[2]), 0)), i8(max(int(d_r[1]), 0)), i8(max(int(d_r[0]), 0)))
	dneg_r := x86._mm_set_epi8(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		i8(max(-int(d_r[3]), 0)), i8(max(-int(d_r[2]), 0)), i8(max(-int(d_r[1]), 0)), i8(max(-int(d_r[0]), 0)))
	dpos_g := x86._mm_set_epi8(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		i8(max(int(d_g[3]), 0)), i8(max(int(d_g[2]), 0)), i8(max(int(d_g[1]), 0)), i8(max(int(d_g[0]), 0)))
	dneg_g := x86._mm_set_epi8(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		i8(max(-int(d_g[3]), 0)), i8(max(-int(d_g[2]), 0)), i8(max(-int(d_g[1]), 0)), i8(max(-int(d_g[0]), 0)))
	dpos_b := x86._mm_set_epi8(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		i8(max(int(d_b[3]), 0)), i8(max(int(d_b[2]), 0)), i8(max(int(d_b[1]), 0)), i8(max(int(d_b[0]), 0)))
	dneg_b := x86._mm_set_epi8(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		i8(max(-int(d_b[3]), 0)), i8(max(-int(d_b[2]), 0)), i8(max(-int(d_b[1]), 0)), i8(max(-int(d_b[0]), 0)))

	r_shift := p.r_shift
	g_shift := p.g_shift
	r_shr := 8 - p.r_bits
	g_shr := 8 - p.g_bits
	b_shr := 8 - p.b_bits

	for x := 0; x < w; x += 4 {
		if x + 4 > w {
			break
		}
		px := x86._mm_loadu_si128(cast(^x86.__m128i)&rgba[x * 4])

		rb := x86._mm_shuffle_epi8(px, shuf_mask_r())
		gb := x86._mm_shuffle_epi8(px, shuf_mask_g())
		bb := x86._mm_shuffle_epi8(px, shuf_mask_b())

		rb = x86._mm_subs_epu8(x86._mm_adds_epu8(rb, dpos_r), dneg_r)
		gb = x86._mm_subs_epu8(x86._mm_adds_epu8(gb, dpos_g), dneg_g)
		bb = x86._mm_subs_epu8(x86._mm_adds_epu8(bb, dpos_b), dneg_b)

		r16 := x86._mm_unpacklo_epi8(rb, zero)
		g16 := x86._mm_unpacklo_epi8(gb, zero)
		b16 := x86._mm_unpacklo_epi8(bb, zero)

		r16 = x86._mm_sll_epi16(x86._mm_srl_epi16(r16, x86._mm_cvtsi32_si128(i32(r_shr))), x86._mm_cvtsi32_si128(i32(r_shift)))
		g16 = x86._mm_sll_epi16(x86._mm_srl_epi16(g16, x86._mm_cvtsi32_si128(i32(g_shr))), x86._mm_cvtsi32_si128(i32(g_shift)))
		b16 = x86._mm_srl_epi16(b16, x86._mm_cvtsi32_si128(i32(b_shr)))

		ci16 := x86._mm_or_si128(x86._mm_or_si128(r16, g16), b16)
		ci8 := x86._mm_packus_epi16(ci16, zero)

		tmp: [16]u8
		x86._mm_storeu_si128(cast(^x86.__m128i)&tmp[0], ci8)
		dst[x] = tmp[0]
		dst[x+1] = tmp[1]
		dst[x+2] = tmp[2]
		dst[x+3] = tmp[3]
	}
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
	epoch:      u16,
	pixel_hash: u64,
}

strip_state_reset :: proc(st: ^StripState, strip_cap, nc: int) {
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
	for i in 0..<strip_cap {
		st.buf[i] = 0
	}
	clear(&st.dirty)
}

write_sixel_num :: proc(b: ^Buffer, n: int) {
	if n >= 100 {
		write_byte(b, u8(0x30 + n/100))
		write_byte(b, u8(0x30 + (n%100)/10))
		write_byte(b, u8(0x30 + n%10))
	} else if n >= 10 {
		write_byte(b, u8(0x30 + n/10))
		write_byte(b, u8(0x30 + n%10))
	} else {
		write_byte(b, u8(0x30 + n))
	}
}

encode_strip :: proc(out: ^Buffer, st: ^StripState, sixel_row, width: int) {
	if sixel_row > 0 {
		write_byte(out, 0x2d)
	}
	for c in st.dirty {
		base := c * width
		row := st.buf[base:base+width]
		write_byte(out, 0x24)
		write_byte(out, 0x23)
		write_sixel_num(out, c + 1)
		last_ch: u8 = 0
		run_count := 0
		for x := 0; x <= width; x += 1 {
			ch: u8 = 0xff
			if x < width {
				ch = row[x]
			}
			if ch != last_ch || run_count == 255 {
				if run_count > 0 {
					if run_count > 1 {
						write_byte(out, 0x21)
						write_sixel_num(out, run_count)
					}
					write_byte(out, last_ch + 63)
				}
				last_ch = ch
				run_count = 1
			} else {
				run_count += 1
			}
		}
	}
	clear(&st.dirty)
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

strip_pool_run :: proc(ctx: ^StripWorkerCtx) {
	st: StripState
	row_ci := make([]u8, ctx.width)
	defer free(&row_ci[0])
	buf: Buffer
	buffer_init(&buf, ctx.width * 64)
	defer free(&buf.buf[0])

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
		p_hash := strip_pixel_hash_rgba(ctx.rgba, width, y_start, y_end)

		if ctx.cache.hashes[srow] == p_hash && len(ctx.cache.rle[srow]) > 0 {
			continue
		}

		buffer_reset(&buf)
		strip_state_reset(&st, nc * width, nc)

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
			for x in 0..<width {
				ci := int(row_ci[x])
				if st.seen[ci] != st.epoch {
					st.seen[ci] = st.epoch
					append(&st.dirty, ci)
				}
				st.buf[ci*width+x] |= bit
			}
			row_offset += width * 4
		}

		encode_strip(&buf, &st, srow, width)

		ctx.cache.hashes[srow] = p_hash
		clear(&ctx.cache.rle[srow])
		append(&ctx.cache.rle[srow], ..buf.buf[:buf.pos])
	}
}

strip_pool_worker :: proc(data: rawptr) {
	pool := (^StripPool)(data)
	defer sync.sema_post(&pool.exited)
	for {
		sync.sema_wait(&pool.start)
		if sync.atomic_load(&pool.stopped) != 0 {
			return
		}
		strip_pool_run(&pool.ctx)
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

	write_bytes(out, []u8{0x1b, 0x50, 0x30, 0x3b, 0x30, 0x3b, 0x38, 0x71, 0x22})
	write_sixel_num(out, width)
	write_byte(out, ';')
	write_sixel_num(out, height)
	write_bytes(out, []u8{0x3b, 0x31, 0x3b, 0x31})
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

	n_workers := len(pool.workers)
	pool.ctx = StripWorkerCtx{
		rgba = rgba,
		width = width,
		height = height,
		pal = pal,
		cache = cache,
		total_strips = total_strips,
	}
	sync.atomic_store(&pool.ctx.next_job, 0)
	sync.sema_post(&pool.start, n_workers)
	for _ in 0..<n_workers {
		sync.sema_wait(&pool.done)
	}

	for srow in 0..<total_strips {
		write_bytes(out, cache.rle[srow][:])
	}
	write_bytes(out, []u8{0x1b, 0x5c})
}
