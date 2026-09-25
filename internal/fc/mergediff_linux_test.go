package fc

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeDiffCopiesWrittenExtentsIncludingZeroPages(t *testing.T) {
	dir := t.TempDir()
	base, diff := filepath.Join(dir, "vm.mem"), filepath.Join(dir, "diff.mem")

	const size = 64 << 20

	old := bytes.Repeat([]byte{0xAA}, size)
	if err := os.WriteFile(base, old, 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(diff)
	if err != nil {
		t.Fatal(err)
	}

	// Two dirtied regions: one with new bytes, one dirtied to ALL ZEROES - the page a
	// non-zero scan would skip and restore stale.
	page := bytes.Repeat([]byte{0x55}, 1<<20)
	zero := make([]byte, 1<<20)

	if _, err := f.WriteAt(page, 8<<20); err != nil {
		t.Fatal(err)
	}

	if _, err := f.WriteAt(zero, 32<<20); err != nil {
		t.Fatal(err)
	}

	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}

	f.Close()

	if err := MergeDiff(diff, base); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(base)

	if !bytes.Equal(got[8<<20:9<<20], page) {
		t.Fatal("dirty page not merged")
	}

	if !bytes.Equal(got[32<<20:33<<20], zero) {
		t.Fatal("a page dirtied to zeroes kept its stale contents")
	}

	if !bytes.Equal(got[:8<<20], old[:8<<20]) || !bytes.Equal(got[40<<20:], old[40<<20:]) {
		t.Fatal("a hole in the diff overwrote the base")
	}
}
