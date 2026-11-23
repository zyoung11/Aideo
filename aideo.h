/* aideo.h - C ABI for the Aideo terminal rendering engine.
 *
 * Feed RGBA or YUV420P frames and the engine draws them into a terminal
 * file descriptor as sixel graphics with incremental band updates.
 */
#ifndef AIDEO_H
#define AIDEO_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct aideo_engine aideo_engine;

typedef struct aideo_config {
	int fd;          /* output file descriptor, usually a tty */
	int src_w;       /* source frame width, used by aideo_push_yuv */
	int src_h;       /* source frame height, used by aideo_push_yuv */
	int out_w;       /* drawn width in pixels */
	int out_h;       /* drawn height in pixels */
	int colors;      /* palette size: 2, 8, 16, 32, 64, 128 or 256 */
	int origin_row;  /* cursor row the picture starts at, from 1 */
	int origin_col;  /* cursor column the picture starts at, from 1 */
	int border_px;   /* solid frame width in pixels, 0 disables */
	int border_color;/* frame color as 0xRRGGBB, 0 means default */
} aideo_config;

/* aideo_open creates an engine and starts its render threads. */
aideo_engine *aideo_open(const aideo_config *cfg);

/* aideo_close stops the threads and releases every resource. */
void aideo_close(aideo_engine *e);

/* aideo_resize changes the drawn size and clears the screen. The next
 * frame is drawn in full. */
void aideo_resize(aideo_engine *e, int out_w, int out_h, int origin_row, int origin_col);

/* aideo_set_colors switches the palette. The next frame is drawn in full. */
void aideo_set_colors(aideo_engine *e, int colors);

/* aideo_push_rgba queues one RGBA frame with the given row stride. The
 * call returns immediately and the engine draws asynchronously. */
void aideo_push_rgba(aideo_engine *e, const uint8_t *rgba, int stride);

/* aideo_push_yuv queues one yuv420p frame with the given plane strides.
 * The frame is scaled to the configured output size. */
void aideo_push_yuv(aideo_engine *e, const uint8_t *y, const uint8_t *u,
	const uint8_t *v, int stride_y, int stride_c);

/* aideo_clear erases the picture area on the terminal. */
void aideo_clear(aideo_engine *e);

/* aideo_repaint forces the next frame to be drawn in full without
 * clearing the screen first. */
void aideo_repaint(aideo_engine *e);

/* aideo_stdout_lock serializes one caller's terminal writes against
 * every render thread so text and sixel streams never interleave. */
void aideo_stdout_lock(void);

/* aideo_stdout_unlock releases the terminal write lock. */
void aideo_stdout_unlock(void);

#ifdef __cplusplus
}
#endif

#endif /* AIDEO_H */
