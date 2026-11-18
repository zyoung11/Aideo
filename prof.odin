package aideo

// import "core:fmt"
// import "core:os"
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

// prof_report writes the stage timings to aideo_prof.txt in the working
// directory. Temporary tool for performance tuning.
// prof_report :: proc() {
// 	f, _ := os.create("aideo_prof.txt")
// 	if f == nil {
// 		return
// 	}
// 	defer os.close(f)
// 	os.write_string(f, "=== profile report ===\n")
// 	for i in 0..<PROF_SLOTS {
// 		c := sync.atomic_load(&prof[i].count)
// 		t := sync.atomic_load(&prof[i].total_ns)
// 		if c == 0 {
// 			continue
// 		}
// 		os.write_string(f, fmt.tprintf("%-22s count=%-8d avg=%.3fms total=%.1fms\n",
// 			prof_names[i], c, f64(t)/f64(c)/1e6, f64(t)/1e6))
// 	}
// }
