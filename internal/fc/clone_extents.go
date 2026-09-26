//go:build linux || darwin

package fc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// extentCopy copies only in's data extents to the same offsets in out, found with
// SEEK_DATA/SEEK_HOLE, and leaves the rest a hole. The copy of each extent is io.Copy from a
// limited *os.File to an *os.File, which on Linux is copy_file_range: in the kernel, never through
// this process, and a reflink where the filesystem can. errNoExtents is a filesystem that cannot
// say where its data is, and the caller falls back to reading everything.
func extentCopy(out, in *os.File) error {
	st, err := in.Stat()
	if err != nil {
		return err
	}

	fd := int(in.Fd())

	var off int64

	for off < st.Size() {
		start, err := syscall.Seek(fd, off, seekData)
		if errors.Is(err, syscall.ENXIO) {
			break // no data after off
		}

		if err != nil {
			if off == 0 && (errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)) {
				return errNoExtents
			}

			return fmt.Errorf("SEEK_DATA: %w", err)
		}

		end, err := syscall.Seek(fd, start, seekHole)
		if err != nil {
			return fmt.Errorf("SEEK_HOLE: %w", err)
		}

		if _, err := in.Seek(start, io.SeekStart); err != nil {
			return err
		}

		if _, err := out.Seek(start, io.SeekStart); err != nil {
			return err
		}

		if _, err := io.Copy(out, io.LimitReader(in, end-start)); err != nil {
			return err
		}

		off = end
	}

	// A trailing hole is only a seek, which does not extend the file; truncate does.
	return out.Truncate(st.Size())
}
