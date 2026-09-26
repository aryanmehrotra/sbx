//go:build !windows

package provider

import (
	"os"
	"syscall"
)

// allocated is the bytes a file occupies on disk: its blocks, not its length. A snapshot's memory
// file is sparse wherever the guest never touched a page, and a reflinked root filesystem is
// counted once per copy, as du counts it.
func allocated(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}

	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}

	return fi.Size()
}

// links is how many names the file has; 1 where the platform does not say.
func links(path string) uint64 {
	fi, err := os.Lstat(path)
	if err != nil {
		return 1
	}

	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // uint16 on darwin, uint64 on linux
	}

	return 1
}

// sameFilesystem reports whether a and b are on one filesystem (one st_dev); false when either
// cannot be read.
func sameFilesystem(a, b string) bool {
	x, err1 := os.Stat(a)
	y, err2 := os.Stat(b)

	if err1 != nil || err2 != nil {
		return false
	}

	sx, ok1 := x.Sys().(*syscall.Stat_t)
	sy, ok2 := y.Sys().(*syscall.Stat_t)

	return ok1 && ok2 && sx.Dev == sy.Dev
}
