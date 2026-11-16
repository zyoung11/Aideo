package aideo

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
}
