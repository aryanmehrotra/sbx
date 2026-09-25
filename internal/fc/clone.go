package fc

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// Clone modes, as CloneFile reports them.
const (
	CloneReflink = "reflink"
	CloneCopy    = "copy"
)

// CloneFile makes dst a private, writable copy of src and says how.
//
// A reflink (FICLONE) first: on btrfs and XFS - and on overlay/bcachefs where supported - it
// shares every block until one side writes, so a clone of a 2 GiB root filesystem is a
// metadata operation. Where the filesystem cannot (ext4 has no reflinks), a sparse copy: blocks
// that are all zeroes become holes, so a mostly-empty rootfs costs what it holds rather than its
// apparent size. Which one happened is returned so it can be reported, never assumed.
func CloneFile(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}

	if err := reflink(out, in); err == nil {
		return CloneReflink, out.Close()
	}

	if err := sparseCopy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)

		return "", err
	}

	return CloneCopy, out.Close()
}

// sparseCopy copies in to out, seeking over zero blocks instead of writing them.
func sparseCopy(out, in *os.File) error {
	st, err := in.Stat()
	if err != nil {
		return err
	}

	const block = 1 << 20

	buf := make([]byte, block)
	zero := make([]byte, block)

	var off int64

	for {
		n, err := io.ReadFull(in, buf)
		if n > 0 {
			if bytes.Equal(buf[:n], zero[:n]) {
				if _, err := out.Seek(int64(n), io.SeekCurrent); err != nil {
					return err
				}
			} else if _, err := out.Write(buf[:n]); err != nil {
				return err
			}

			off += int64(n)
		}

		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}

		if err != nil {
			return err
		}
	}

	// A trailing hole is only a seek, which does not extend the file; truncate does.
	return out.Truncate(st.Size())
}
