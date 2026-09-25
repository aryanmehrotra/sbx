package provider

import (
	"os"
	"syscall"
)

// fileLock takes an exclusive flock on path for the life of one VM operation, so two sbx
// processes - the daemon sleeping a VM, a CLI removing it - never interleave. A directory that
// does not exist yet (mid-create) or any other failure to lock degrades to the in-process mutex
// alone, which is what one process needs anyway.
func fileLock(path string) func() {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
