//go:build !(darwin || freebsd || netbsd || openbsd || dragonfly || linux)

package tui

// Everywhere else - windows, js, plan9 - there is no raw mode here.
//
// Dialling a Windows named pipe already needs a dialer outside the standard library, which is
// why this tool asks to be run under WSL2 there; a console API binding would be the second
// such dependency for the same platform. The dashboard says so and prints the table once
// instead, which is still useful and is honest about what it is not doing.

import "os"

type state struct{}

func makeRaw(uintptr) (*state, error) { return nil, errUnsupported }

func restore(uintptr, *state) error { return nil }

func size(uintptr) (rows, cols int) { return 24, 80 }

const supported = false

// IsTerminal cannot be answered without a console API here, so it says no and the caller
// takes its non-interactive path.
func IsTerminal(*os.File) bool { return false }
