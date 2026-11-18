package aideo

import "core:sync"

PROF_SLOTS :: 12

PROF_DECODE_READ  :: 0
PROF_DECODE_RECV  :: 1
PROF_DECODE_SWS   :: 2
PROF_ENCODE_HASH  :: 3
PROF_ENCODE_SCAT :: 4
PROF_ENCODE_RLE   :: 5
PROF_ENCODE_ASM   :: 6
PROF_WRITE        :: 7
PROF_AUDIO_DECODE :: 8
PROF_QUEUE_WAIT   :: 9
PROF_MAIN_LOOP    :: 10
PROF_FRAME_GAP    :: 11

ProfSlot :: struct {
	count: i64,
	total_ns: i64,
	max_ns: i64,
}

prof: [PROF_SLOTS]ProfSlot

prof_names := [PROF_SLOTS]string{
	"decode: av_read_frame",
	"decode: send/recv",
	"decode: sws_scale",
	"encode: hash",
	"encode: scatter",
	"encode: rle",
	"encode: assemble",
	"write: stdout",
	"audio: decode+convert",
	"main: queue wait",
	"main: loop body",
	"main: frame gap",
}

prof_add :: proc "contextless" (slot: int, ns: i64) {
	sync.atomic_add(&prof[slot].count, 1)
	sync.atomic_add(&prof[slot].total_ns, ns)
}
