package tui

import "errors"

// ErrUnsupported is returned where this build cannot drive a terminal. Callers print the
// static view rather than failing.
var ErrUnsupported = errors.New("an interactive terminal is not supported on this platform; " +
	"under WSL2 it is, and `sbx list` prints the same information anywhere")

var errUnsupported = ErrUnsupported
