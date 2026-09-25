package fchost

import "os"

// processAlive: on Windows FindProcess opens a handle, which fails for a process that is gone.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	_ = p.Release()

	return true
}
