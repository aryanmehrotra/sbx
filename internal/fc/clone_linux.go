package fc

import (
	"os"
	"syscall"
)

// ficlone is _IOW(0x94, 9, int): the reflink ioctl, the one `cp --reflink` uses.
const ficlone = 0x40049409

func reflink(dst, src *os.File) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficlone, src.Fd())
	if errno != 0 {
		return errno
	}

	return nil
}
