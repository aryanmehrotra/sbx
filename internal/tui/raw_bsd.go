//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package tui

import "syscall"

// The BSDs, macOS included, spell the terminal-attribute ioctls this way. Linux uses TCGETS
// and TCSETS for the same two operations; see raw_linux.go.
const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)
