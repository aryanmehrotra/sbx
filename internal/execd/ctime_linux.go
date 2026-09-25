//go:build linux

package execd

import (
	"os"
	"syscall"
	"time"
)

// createdAt is the inode change time, which is what upstream reports as created_at on Linux:
// ext4 and overlayfs keep a birth time, but only statx exposes it and the syscall package does
// not wrap statx.
func createdAt(fi os.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec)) // int32 on 386
	}

	return fi.ModTime()
}
