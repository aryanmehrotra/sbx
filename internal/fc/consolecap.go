package fc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// A VM's console.log and vmm.log are appended to by firecracker for the VM's whole life - the
// first with whatever the guest prints on its serial console. Unbounded, a guest printing in a
// loop fills the host's disk. They are cut back to their newest ConsoleKeep bytes once past
// ConsoleMax: at every launch, and on every daemon reconcile (CapLogs).
const (
	ConsoleMax  = 16 << 20
	ConsoleKeep = 4 << 20
)

// CapLogs trims a VM directory's console.log and vmm.log if either is past ConsoleMax.
func CapLogs(dir string) error {
	return errors.Join(
		capFile(filepath.Join(dir, ConsoleName), ConsoleMax, ConsoleKeep),
		capFile(filepath.Join(dir, VMMLogName), ConsoleMax, ConsoleKeep),
	)
}

// capFile cuts path back to its last keep bytes, in whole lines, once it is past limit. In place,
// not by rename: firecracker holds the file open O_APPEND, so after the truncate its next write
// lands at the new end rather than into a file nobody reads. A line written between the read and
// the truncate can be lost; for a console, that is the price of not stopping the VM to trim it.
func capFile(path string, limit, keep int64) error {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && st.Size() <= limit) {
		return nil
	}

	if err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}

	tail := make([]byte, keep)
	n, err := f.ReadAt(tail, st.Size()-keep)
	f.Close()

	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	tail = tail[:n]
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:] // from the first whole line
	}

	if err := os.Truncate(path, 0); err != nil {
		return err
	}

	w, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer w.Close()

	_, err = fmt.Fprintf(w, "[sbx: %s was past %d KiB; trimmed to its newest %d KiB]\n%s",
		filepath.Base(path), limit>>10, len(tail)>>10, tail)

	return err
}
