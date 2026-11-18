package aideo

import "core:c"
import "core:fmt"

ci_test :: proc() {
	p := get_palette(64)
	w := 16
	rgba := make([]u8, w * 4)
	for x in 0..<w {
		rgba[x*4+0] = u8(x * 16)
		rgba[x*4+1] = u8(255 - x * 16)
		rgba[x*4+2] = u8(x * 8 + 3)
		rgba[x*4+3] = 255
	}
	simd_out := make([]u8, w)
	scalar_out := make([]u8, w)
	yb4 := (2 & 3) << 2
	quantize_row_uniform_simd(simd_out, rgba, w, yb4, p)
	quantize_row_uniform_scalar(scalar_out, rgba, w, yb4, 0, p)
	for x in 0..<w {
		mark := "OK"
		if simd_out[x] != scalar_out[x] {
			mark = "DIFF"
		}
		fmt.printfln("x=%2d rgba=(%3d,%3d,%3d) simd=%3d scalar=%3d %s", x, rgba[x*4], rgba[x*4+1], rgba[x*4+2], simd_out[x], scalar_out[x], mark)
	}
	qw := [?]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 12, 15, 16, 17, 1097, 2190, 2193, 3840}
	bad := 0
	for tw in qw {
		tbuf := make([]u8, tw * 4)
		for i in 0..<len(tbuf) {
			tbuf[i] = u8((i * 37 + 11) % 251)
		}
		a := make([]u8, tw)
		b := make([]u8, tw)
		for ty in 0..<4 {
			yb := (ty & 3) << 2
			quantize_row_uniform_simd(a, tbuf, tw, yb, p)
			quantize_row_uniform_scalar(a, tbuf, tw, yb, tw &~ 7, p)
			quantize_row_uniform_scalar(b, tbuf, tw, yb, 0, p)
			for i in 0..<tw {
				if a[i] != b[i] {
					bad += 1
					fmt.printfln("quantize width=%d y=%d x=%d simd_tail=%d scalar=%d DIFF", tw, ty, i, a[i], b[i])
				}
			}
		}
	}
	fmt.printfln("quantize width sweep: %s", bad == 0 ? "OK" : "DIFF")
	bands := [][2]int{{0, 12}, {0, 1}, {3, 9}, {5, 6}, {11, 12}}
	widths := []int{1, 7, 8, 16, 37}
	for w in widths {
		buf := make([]u8, w * 12 * 4)
		for i in 0..<len(buf) {
			buf[i] = u8((i * 37 + i / 7 + 11) % 251)
		}
		for band in bands {
			ref := strip_pixel_hash_rgba(buf, w, band[0], band[1])
			simd := hash_strip_avx2(&buf[0], c.int(w), c.int(band[0]), c.int(band[1]))
			mark := "OK"
			if ref != simd {
				mark = "DIFF"
			}
			fmt.printfln("hash w=%2d y=%2d..%-2d scalar=%016x avx2=%016x %s", w, band[0], band[1], ref, simd, mark)
		}
	}
}
