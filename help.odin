package aideo

import "core:fmt"

HELP_MAUVE  :: "\x1b[38;2;198;160;246m"
HELP_BLUE   :: "\x1b[38;2;138;173;244m"
HELP_GREEN  :: "\x1b[38;2;166;218;149m"
HELP_YELLOW :: "\x1b[38;2;238;212;159m"
HELP_RED    :: "\x1b[38;2;237;135;150m"
HELP_BOLD   :: "\x1b[1m"
HELP_RESET  :: "\x1b[0m"

VERSION :: "v0.1.0"

// print_version renders the version line with the name in blue and the
// version in green.
print_version :: proc() {
	fmt.printfln("%saideo%s %s%s%s", HELP_BLUE, HELP_RESET, HELP_GREEN, VERSION, HELP_RESET)
}

// help_line prints one two column row with the left column padded to width.
help_line :: proc(color, name: string, width: int, right: string) {
	spaces := "                                                            "
	n := width - len(name)
	if n < 0 {
		n = 0
	}
	if n > len(spaces) {
		n = len(spaces)
	}
	fmt.printfln("  %s%s%s%s %s", color, name, spaces[:n], HELP_RESET, right)
}

// cli_error prints one error message in the log error style.
cli_error :: proc(msg: string) {
	fmt.printfln("%s[-]%s %s", HELP_RED, HELP_RESET, msg)
}

// print_help renders the help page with the terminal output colors.
print_help :: proc() {
	title := fmt.tprintf("%s%saideo%s", HELP_BOLD, HELP_BLUE, HELP_RESET)
	section := fmt.tprintf("%s%s", HELP_BOLD, HELP_MAUVE)

	fmt.printfln("")
	fmt.printfln("%s — terminal sixel video player", title)
	fmt.printfln("")
	fmt.printfln("%sUSAGE:%s", section, HELP_RESET)
	fmt.printfln("  %saideo%s %s[options]%s %s<file>%s", HELP_BLUE, HELP_RESET, HELP_YELLOW, HELP_RESET, HELP_YELLOW, HELP_RESET)
	fmt.printfln("  %saideo%s %s--help%s", HELP_BLUE, HELP_RESET, HELP_YELLOW, HELP_RESET)
	fmt.printfln("")
	fmt.printfln("%sOPTIONS:%s", section, HELP_RESET)
	help_line(HELP_YELLOW, "-c, --colors <n>", 22, "color count, one of 2 8 16 32 64 128 256")
	help_line(HELP_YELLOW, "-v, --volume <n>", 22, "volume percent from 0 to 100")
	help_line(HELP_YELLOW, "-t, --start <time>", 22, "start position, seconds or mm:ss or hh:mm:ss")
	help_line(HELP_YELLOW, "-s, --size <mode>", 22, "display size, full or half")
	help_line(HELP_YELLOW, "-p, --profile", 22, "write stage timings to aideo_prof.txt on exit")
	help_line(HELP_YELLOW, "-h, --help", 22, "show this help page")
	help_line(HELP_YELLOW, "    --version", 22, "show the version")
	fmt.printfln("")
	fmt.printfln("%sTARGET:%s", section, HELP_RESET)
	help_line(HELP_YELLOW, "<file>", 22, "path to a video, animation or image file")
	fmt.printfln("")
	fmt.printfln("%sKEYS:%s", section, HELP_RESET)
	help_line(HELP_GREEN, "q, Esc", 22, "quit")
	help_line(HELP_GREEN, "space", 22, "pause or resume")
	help_line(HELP_GREEN, "j, k", 22, "decrease or increase color count")
	help_line(HELP_GREEN, "Up, Down", 22, "lower or raise volume")
	help_line(HELP_GREEN, "m", 22, "toggle mute")
	help_line(HELP_GREEN, "Left, Right", 22, "seek 5 seconds backward or forward")
	help_line(HELP_GREEN, "Tab", 22, "show or hide the status label")
	help_line(HELP_GREEN, "f", 22, "toggle full or half size picture")
	fmt.printfln("")
	fmt.printfln("%sEXAMPLES:%s", section, HELP_RESET)
	fmt.printfln("  %saideo video.mp4%s", HELP_BLUE, HELP_RESET)
	fmt.printfln("  %saideo -c 256 -v 80 -t 1:30 -s half concert.mkv%s", HELP_BLUE, HELP_RESET)
	fmt.printfln("")
}

// parse_time parses a playback position written as seconds, mm:ss or hh:mm:ss.
parse_time :: proc(s: string) -> (f64, bool) {
	parts: [3]f64
	n := 0
	val := 0.0
	seen := false
	i := 0
	for i < len(s) {
		ch := s[i]
		if ch >= '0' && ch <= '9' {
			val = val * 10.0 + f64(ch - '0')
			seen = true
			i += 1
		} else if ch == '.' {
			if !seen {
				return 0, false
			}
			i += 1
			scale := 0.1
			digits := 0
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				val += f64(s[i] - '0') * scale
				scale /= 10.0
				digits += 1
				i += 1
			}
			if digits == 0 {
				return 0, false
			}
		} else if ch == ':' {
			if !seen || n >= 2 {
				return 0, false
			}
			parts[n] = val
			n += 1
			val = 0.0
			seen = false
			i += 1
		} else {
			return 0, false
		}
	}
	if !seen {
		return 0, false
	}
	parts[n] = val
	n += 1
	total := 0.0
	for k in 0..<n {
		total = total * 60.0 + parts[k]
	}
	return total, true
}
