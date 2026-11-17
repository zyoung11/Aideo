package aideo

import "core:fmt"
import "core:os"
import "core:strconv"
import "core:time"

ci_bench :: proc() {
	w, h, colors := 1280, 720, 64
	if len(os.args) >= 5 {
		v1, _ := strconv.parse_int(os.args[2])
		v2, _ := strconv.parse_int(os.args[3])
		v3, _ := strconv.parse_int(os.args[4])
		w, h, colors = int(v1), int(v2), int(v3)
	}
	p := get_palette(colors)
	rgba := make([]u8, w * h * 4)
	for i in 0..<w * h {
		rgba[i*4+0] = u8(i % 256)
		rgba[i*4+1] = u8((i / 3) % 256)
		rgba[i*4+2] = u8((i / 7) % 256)
		rgba[i*4+3] = 255
	}
	row := rgba[0:w*4]
	dst := make([]u8, w)

	t0 := time.now()
	for _ in 0..<200 {
		quantize_row_uniform_simd(dst, row, w, 0, p)
	}
	t_simd := time.duration_seconds(time.since(t0))

	t0 = time.now()
	for _ in 0..<200 {
		quantize_row_uniform_scalar(dst, row, w, 0, 0, p)
	}
	t_scalar := time.duration_seconds(time.since(t0))

	cache: StripCache
	strip_cache_init(&cache, (h + 5) / 6)
	out := Buffer{}
	buffer_init(&out, w * h)
	pool: StripPool
	strip_pool_init(&pool, cpu_count())
	defer strip_pool_stop(&pool)

	t0 = time.now()
	sixel_encode(&pool, &out, rgba, w, h, p, &cache)
	t_first := time.duration_seconds(time.since(t0))
	first_len := out.pos

	buffer_reset(&out)
	t0 = time.now()
	sixel_encode(&pool, &out, rgba, w, h, p, &cache)
	t_cached := time.duration_seconds(time.since(t0))

	fmt.printfln("quantize row(1280px) x200: simd=%.4fs scalar=%.4fs", t_simd, t_scalar)
	fmt.printfln("encode 1280x720: first=%.4fs(%d bytes) cached=%.4fs", t_first, first_len, t_cached)
}
