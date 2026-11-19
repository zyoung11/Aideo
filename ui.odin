package aideo

import "core:c"
import "core:fmt"

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
// false the area is halved in both dimensions. The height is capped one
// row short of the terminal height so a sixel linefeed never reaches the
// bottom row, which would scroll the screen on foot.
fit_disp :: proc(src_w, src_h, cols, rows, cell_w, cell_h: int, full: bool) -> (int, int) {
	w := cols * cell_w
	h := rows * cell_h
	if !full {
		w /= 2
		h /= 2
	}
	if rows > 1 && (h + cell_h - 1) / cell_h >= rows {
		h = (rows - 1) * cell_h
		w = cols * cell_w
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
