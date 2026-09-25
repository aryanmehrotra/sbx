//go:build !linux

package guestinit

import "runtime"

// Main refuses: a Firecracker guest is Linux by definition.
func Main(_ []string) int {
	return fail("runs only as PID 1 inside a Firecracker VM, which is Linux; this is " + runtime.GOOS)
}
