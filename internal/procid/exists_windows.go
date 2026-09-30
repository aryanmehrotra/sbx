package procid

import "os"

// exists: on Windows FindProcess opens a handle, which fails for a process that is gone.
func exists(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	_ = proc.Release()

	return true
}
