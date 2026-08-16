//go:build linux

package tui

import "syscall"

// Linux's names for the same two ioctls the BSDs call TIOCGETA and TIOCSETA.
const (
	ioctlGetTermios = syscall.TCGETS
	ioctlSetTermios = syscall.TCSETS
)
