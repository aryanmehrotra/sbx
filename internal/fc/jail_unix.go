//go:build unix

package fc

import (
	"os"
	"syscall"
)

// linkCount is how many names the file has; 0 where the platform does not say.
func linkCount(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // uint16 on darwin, uint64 on linux
	}

	return 0
}

// fileOwner is the file's uid, and whether the platform says.
func fileOwner(fi os.FileInfo) (int, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), true
	}

	return 0, false
}
