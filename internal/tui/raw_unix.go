//go:build darwin || freebsd || netbsd || openbsd || dragonfly || linux

package tui

// Raw mode, by hand.
//
// A dashboard needs single keypresses: j should move the cursor, not sit in a buffer waiting
// for enter. That means turning off the terminal's line discipline, which is an ioctl, which
// is why golang.org/x/term exists.
//
// This project's root module has no dependencies outside the standard library, and that is a
// claim gated in CI rather than a preference, so the ioctl is here. It is about thirty lines
// and the only per-platform part is the two constant names, which differ between Linux and
// the BSDs and are in the two files beside this one.

import (
	"os"
	"syscall"
	"unsafe"
)

// state is a saved terminal configuration, to be put back exactly as it was found.
type state struct {
	termios syscall.Termios
}

func ioctl(fd uintptr, req uintptr, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}

	return nil
}

// makeRaw puts the terminal into raw mode and returns what it was, for restoring.
//
// The flags are the ones cfmakeraw sets, minus the output processing: leaving OPOST on means
// a bare \n still moves to the start of the next line, so every string this package writes
// does not have to carry \r\n. Nothing here needs byte-exact output control.
func makeRaw(fd uintptr) (*state, error) {
	var old syscall.Termios

	if err := ioctl(fd, ioctlGetTermios, &old); err != nil {
		return nil, err
	}

	raw := old

	// Input: no CR-to-NL translation, no XON/XOFF, no break signal, no parity checking.
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON

	// Local: no echo, no line buffering, no signal generation from ^C and friends. Signals
	// off is why this package installs its own handler for them.
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN

	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8

	// One byte is enough to return, and never block forever waiting for it: a redraw on a
	// timer has to be able to happen while nobody is typing.
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 1 // tenths of a second

	if err := ioctl(fd, ioctlSetTermios, &raw); err != nil {
		return nil, err
	}

	return &state{termios: old}, nil
}

// restore puts the terminal back. Called from a defer and from the signal handler, because a
// terminal left in raw mode after a crash is one the user has to blindly type `reset` into.
func restore(fd uintptr, s *state) error {
	if s == nil {
		return nil
	}

	return ioctl(fd, ioctlSetTermios, &s.termios)
}

// winsize is the kernel's struct for TIOCGWINSZ.
type winsize struct {
	rows, cols     uint16
	xpixel, ypixel uint16
}

// size returns the terminal's rows and columns, falling back to something usable rather than
// failing: a dashboard drawn at 80x24 on a terminal that would not answer is better than no
// dashboard.
func size(fd uintptr) (rows, cols int) {
	var ws winsize

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd,
		uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))

	if errno != 0 || ws.rows == 0 || ws.cols == 0 {
		return 24, 80
	}

	return int(ws.rows), int(ws.cols)
}

// supported reports whether this build can put a terminal into raw mode at all.
const supported = true

// IsTerminal reports whether f is something a person is looking at, rather than a pipe.
func IsTerminal(f *os.File) bool {
	var t syscall.Termios

	return ioctl(f.Fd(), ioctlGetTermios, &t) == nil
}
