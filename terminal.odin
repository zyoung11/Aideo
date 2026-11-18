package aideo

import "core:c"
import "core:fmt"
import "core:os"
import "core:sync"
import "core:thread"

SIGWINCH :: 28

TIOCGWINSZ :: 0x5413
TCSANOW    :: 0

Termios :: struct {
	iflag:  u32,
	oflag:  u32,
	cflag:  u32,
	lflag:  u32,
	line:   u8,
	cc:     [32]u8,
	ispeed: u32,
	ospeed: u32,
}

WinSize :: struct {
	row:    u16,
	col:    u16,
	xpixel: u16,
	ypixel: u16,
}

foreign import clib {
	"system:libc.so.6",
}

foreign clib {
	tcgetattr :: proc(fd: c.int, t: ^Termios) -> c.int ---
	tcsetattr :: proc(fd: c.int, act: c.int, t: ^Termios) -> c.int ---
	ioctl :: proc(fd: c.int, req: u64, arg: rawptr) -> c.int ---
	signal :: proc(sig: c.int, handler: rawptr) -> rawptr ---
	sysconf :: proc(name: c.int) -> c.long ---
	writev :: proc(fd: c.int, iov: [^]IoVec, cnt: c.int) -> c.long ---
}

// IoVec is one scatter gather buffer for writev.
IoVec :: struct {
	iov_base: rawptr,
	iov_len:  u64,
}

// IoVecList collects scatter gather segments with a fixed capacity.
IoVecList :: struct {
	iovs: [4096]IoVec,
	n:    int,
}

// iovs_push appends one segment to the list.
iovs_push :: proc(l: ^IoVecList, data: []u8) {
	if len(data) == 0 || l.n >= len(l.iovs) {
		return
	}
	l.iovs[l.n].iov_base = &data[0]
	l.iovs[l.n].iov_len = u64(len(data))
	l.n += 1
}

// iovs_len returns the total byte count of all segments.
iovs_len :: proc(l: ^IoVecList) -> int {
	t := 0
	for i in 0..<l.n {
		t += int(l.iovs[i].iov_len)
	}
	return t
}

// cpu_count returns the number of online CPUs.
cpu_count :: proc() -> int {
	return max(int(sysconf(84)), 1)
}

term_orig: Termios
term_raw_ok: bool

term_enter_raw :: proc() {
	fd := c.int(0)
	if tcgetattr(fd, &term_orig) != 0 {
		return
	}
	raw := term_orig
	raw.lflag &= ~u32(0x8 | 0x2 | 0x1 | 0x40000)
	raw.iflag &= ~u32(0x100 | 0x400 | 0x2 | 0x10 | 0x1 | 0x200 | 0x20 | 0x8)
	raw.oflag &= ~u32(0x1 | 0x4)
	raw.cc[6] = 1
	raw.cc[5] = 0
	tcsetattr(fd, TCSANOW, &raw)
	term_raw_ok = true
}

term_leave_raw :: proc() {
	if term_raw_ok {
		tcsetattr(c.int(0), TCSANOW, &term_orig)
		term_raw_ok = false
	}
}

term_size :: proc() -> (int, int) {
	ws: WinSize
	if ioctl(c.int(1), TIOCGWINSZ, &ws) != 0 {
		return 80, 24
	}
	return int(ws.col), int(ws.row)
}

term_cell_pixels :: proc() -> (int, int) {
	ws: WinSize
	if ioctl(c.int(1), TIOCGWINSZ, &ws) != 0 || ws.xpixel == 0 || ws.col == 0 {
		return 8, 16
	}
	return int(ws.xpixel) / int(ws.col), int(ws.ypixel) / int(ws.row)
}

winch_flag: i32

winch_handler :: proc "c" (_: c.int) {
	sync.atomic_store(&winch_flag, 1)
}

term_watch_winch :: proc() {
	signal(SIGWINCH, cast(rawptr)winch_handler)
}

term_take_winch :: proc() -> bool {
	if sync.atomic_load(&winch_flag) != 0 {
		sync.atomic_store(&winch_flag, 0)
		return true
	}
	return false
}

KEY_QUIT  :: 0
KEY_PAUSE :: 1
KEY_VOL_UP :: 2
KEY_VOL_DOWN :: 3
KEY_COLOR_UP :: 4
KEY_COLOR_DOWN :: 5
KEY_FPS :: 6
KEY_MUTE :: 8
KEY_SEEK_FWD :: 9
KEY_SEEK_BACK :: 10

key_queue: [32]u8
key_head: i64
key_tail: i64

key_push :: proc(k: u8) {
	head := sync.atomic_load(&key_head)
	tail := sync.atomic_load(&key_tail)
	if head-tail >= i64(len(key_queue)) {
		return
	}
	key_queue[head % i64(len(key_queue))] = k
	sync.atomic_store(&key_head, head + 1)
}

key_pop :: proc() -> (u8, bool) {
	head := sync.atomic_load(&key_head)
	tail := sync.atomic_load(&key_tail)
	if head == tail {
		return 0, false
	}
	k := key_queue[tail % i64(len(key_queue))]
	sync.atomic_store(&key_tail, tail + 1)
	return k, true
}

key_thread :: proc() {
	buf: [64]u8
	for {
		n, err := os.read(os.stdin, buf[:])
		if err != nil || n <= 0 {
			key_push(KEY_QUIT)
			return
		}
		i := 0
		for i < n {
			b := buf[i]
			if b == 27 && i+1 < n && (buf[i+1] == '[' || buf[i+1] == 'O') {
				i += 2
				for i < n && !((buf[i] >= 'A' && buf[i] <= 'Z') || (buf[i] >= 'a' && buf[i] <= 'z') || buf[i] == '~') {
					i += 1
				}
				if i < n {
					switch buf[i] {
					case 'A':
						key_push(KEY_VOL_UP)
					case 'B':
						key_push(KEY_VOL_DOWN)
					case 'C':
						key_push(KEY_SEEK_FWD)
					case 'D':
						key_push(KEY_SEEK_BACK)
					}
				}
				i += 1
				continue
			}
			switch b {
			case 'q', 'Q', 27:
				key_push(KEY_QUIT)
			case ' ':
				key_push(KEY_PAUSE)
			case 'j', 'J':
				key_push(KEY_COLOR_DOWN)
			case 'k', 'K':
				key_push(KEY_COLOR_UP)
			case 9:
				key_push(KEY_FPS)
			case 'm', 'M':
				key_push(KEY_MUTE)
			}
			i += 1
		}
	}
}

key_start :: proc() {
	thread.create_and_start(proc() {
		key_thread()
	})
}
