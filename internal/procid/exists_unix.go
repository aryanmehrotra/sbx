//go:build !windows

package procid

import (
	"errors"
	"os"
	"syscall"
)

// exists asks with signal 0, which delivers nothing. EPERM is a process that exists and
// belongs to another user - alive, so still a possible holder.
func exists(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	err = proc.Signal(syscall.Signal(0))

	return err == nil || errors.Is(err, syscall.EPERM)
}
