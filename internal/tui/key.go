package tui

// Turning bytes from a terminal into keys.
//
// An arrow key is not a byte. It arrives as an escape sequence - ESC [ A for up - and the
// same ESC also means the escape key on its own, which is why a naive reader treats a press
// of Escape as the beginning of a sequence that never arrives and hangs until the next key.
// The fix is the one every terminal program uses: if nothing follows an ESC promptly, it was
// the escape key.

import "io"

// Key is one keypress.
type Key struct {
	Rune rune // the character, for ordinary keys
	Code Code // non-printing keys
}

// Code names the keys that are not characters.
type Code int

const (
	KeyRune Code = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyEnter
	KeyEscape
	KeyCtrlC
	KeyPageUp
	KeyPageDown
	KeyHome
	KeyEnd
)

// Reader decodes keys from a terminal.
type Reader struct {
	in  io.Reader
	buf []byte
}

func NewReader(in io.Reader) *Reader {
	return &Reader{in: in, buf: make([]byte, 0, 16)}
}

// Read blocks until a key is available or the terminal's read timeout expires, in which case
// it returns ok=false so the caller can redraw on its own schedule rather than waiting for
// somebody to type.
//
// Anything left over from a read is kept for the next call. One read can carry several keys -
// a held-down arrow, a paste, or simply two keys pressed inside the hundred-millisecond
// window the terminal waits before returning - and decoding only the first would silently
// drop the rest. That is what made this feel unreliable: pressing down twice and then enter
// registered the arrows and lost the enter.
func (r *Reader) Read() (Key, bool) {
	if len(r.buf) == 0 {
		b := make([]byte, 64)

		n, err := r.in.Read(b)
		if err != nil || n == 0 {
			return Key{}, false
		}

		r.buf = append(r.buf, b[:n]...)
	}

	k, used := decodeOne(r.buf)
	r.buf = r.buf[used:]

	return k, true
}

// decodeOne reads one key off the front of b and reports how many bytes it consumed.
func decodeOne(b []byte) (Key, int) {
	if len(b) == 0 {
		return Key{}, 0
	}

	if b[0] != 27 {
		return decode(b[:1]), 1
	}

	// An escape sequence, or the escape key if nothing usable follows it.
	if len(b) < 3 || (b[1] != '[' && b[1] != 'O') {
		return Key{Code: KeyEscape}, 1
	}

	// CSI sequences ending in ~ carry a number: ESC [ 5 ~ is page up.
	if b[2] >= '0' && b[2] <= '9' {
		for i := 2; i < len(b); i++ {
			if b[i] == '~' {
				return decode(b[:3]), i + 1
			}
		}

		return Key{Code: KeyEscape}, 1
	}

	return decode(b[:3]), 3
}

// decode turns one read's worth of bytes into a key.
//
// Split out from Read so it can be tested without a terminal, which is the only way to test
// it at all: the interesting cases are byte sequences, not typing.
func decode(b []byte) Key {
	if len(b) == 0 {
		return Key{}
	}

	switch b[0] {
	case 3:
		return Key{Code: KeyCtrlC}
	case 13, 10:
		return Key{Code: KeyEnter}
	case 27:
		// Alone, it is the escape key. Followed by [ or O, it is a sequence.
		if len(b) == 1 {
			return Key{Code: KeyEscape}
		}

		return decodeEscape(b)
	}

	return Key{Rune: rune(b[0]), Code: KeyRune}
}

func decodeEscape(b []byte) Key {
	if len(b) < 3 || (b[1] != '[' && b[1] != 'O') {
		return Key{Code: KeyEscape}
	}

	switch b[2] {
	case 'A':
		return Key{Code: KeyUp}
	case 'B':
		return Key{Code: KeyDown}
	case 'C':
		return Key{Code: KeyRight}
	case 'D':
		return Key{Code: KeyLeft}
	case 'H':
		return Key{Code: KeyHome}
	case 'F':
		return Key{Code: KeyEnd}
	case '5':
		return Key{Code: KeyPageUp} // ESC [ 5 ~
	case '6':
		return Key{Code: KeyPageDown}
	}

	return Key{Code: KeyEscape}
}
