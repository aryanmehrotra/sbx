package fc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Linux's lseek whences for sparse files. (macOS has both too, with the numbers swapped, which
// is one more reason this is Linux-only.)
const (
	seekData = 3
	seekHole = 4
)

func mergeDiff(diff, base string) error {
	src, err := os.Open(diff)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(base, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer dst.Close()

	st, err := src.Stat()
	if err != nil {
		return err
	}

	fd := int(src.Fd())

	var off int64

	for off < st.Size() {
		start, err := syscall.Seek(fd, off, seekData)
		if errors.Is(err, syscall.ENXIO) {
			break // no data after off
		}

		if err != nil {
			return fmt.Errorf("SEEK_DATA in %s: %w", diff, err)
		}

		end, err := syscall.Seek(fd, start, seekHole)
		if err != nil {
			return fmt.Errorf("SEEK_HOLE in %s: %w", diff, err)
		}

		if _, err := dst.Seek(start, io.SeekStart); err != nil {
			return err
		}

		if _, err := io.Copy(dst, io.NewSectionReader(src, start, end-start)); err != nil {
			return fmt.Errorf("merging %s into %s: %w", diff, base, err)
		}

		off = end
	}

	return dst.Sync()
}
