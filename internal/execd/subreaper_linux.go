//go:build linux

package execd

import "syscall"

// prSetChildSubreaper is PR_SET_CHILD_SUBREAPER from <linux/prctl.h>; the syscall package does
// not name it.
const prSetChildSubreaper = 36

// becomeSubreaper makes orphans below execd re-parent to execd instead of to PID 1. It matters
// when execd is not PID 1 itself (a runtime that injects its own init, say): without it the
// user entrypoint's orphans would be reaped by somebody else, and execd would be the one
// process that could not see them exit.
func becomeSubreaper() error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0)
	if errno != 0 {
		return errno
	}

	return nil
}
