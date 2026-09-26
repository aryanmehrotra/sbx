//go:build linux || darwin

package fc

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// sparseImage writes a size-byte sparse file holding data only in the given extents, like the
// rootfs cache: mkfs.ext4 wrote the image's files, and the headroom is a hole.
func sparseImage(t testing.TB, size int64, extents map[int64]int) (string, map[int64][]byte) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "src.ext4")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}

	r := rand.New(rand.NewPCG(1, 2))
	want := map[int64][]byte{}

	for off, n := range extents {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.IntN(255) + 1)
		}

		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}

		want[off] = b
	}

	return path, want
}

func allocatedBytes(t testing.TB, path string) int64 {
	t.Helper()

	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}

	return st.Blocks * 512
}

// Without reflinks, a clone copied the whole file through userspace, reading every hole as zeroes
// to find out it was a hole: a 2.5 GB image plus its 2 GiB headroom, per create, which on a CI
// runner (ext4, no reflink) was part of why a code-interpreter create missed its window. A clone
// now copies only the data extents (SEEK_DATA/SEEK_HOLE; copy_file_range on Linux) and leaves the
// holes as holes.
func TestCloneCopiesOnlyTheDataExtents(t *testing.T) {
	const size = 8 << 30 // 8 GiB apparent: reading its holes alone would take seconds

	src, want := sparseImage(t, size, map[int64]int{0: 1 << 20, 3 << 30: 4 << 20, size - 64<<10: 64 << 10})
	dst := filepath.Join(t.TempDir(), "dst.ext4")

	start := time.Now()

	mode, err := CloneFile(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	took := time.Since(start)
	t.Logf("clone of an 8 GiB sparse file holding 5 MiB: %s in %s", mode, took)

	if mode != CloneReflink && mode != CloneExtents {
		t.Fatalf("mode %q: want the data extents copied, not the whole file", mode)
	}

	if took > 2*time.Second {
		t.Fatalf("cloning 5 MiB of data took %s: the holes were read", took)
	}

	st, err := os.Stat(dst)
	if err != nil || st.Size() != size {
		t.Fatalf("size %v, %v", st, err)
	}

	if got := allocatedBytes(t, dst); got > 64<<20 {
		t.Fatalf("the clone allocates %d bytes for 5 MiB of data: the holes were filled", got)
	}

	f, _ := os.Open(dst)
	defer f.Close()

	for off, b := range want {
		got := make([]byte, len(b))
		if _, err := f.ReadAt(got, off); err != nil || !bytes.Equal(got, b) {
			t.Fatalf("extent at %d differs (%v)", off, err)
		}
	}

	hole := make([]byte, 4096)
	if _, err := f.ReadAt(hole, 1<<30); err != nil || !bytes.Equal(hole, make([]byte, 4096)) {
		t.Fatalf("a hole reads back non-zero (%v)", err)
	}
}

// BenchmarkCloneSparse is the number to read on a runner: a 4.5 GiB rootfs-shaped file (2.5 GiB
// of data, 2 GiB of headroom hole), cloned the way a create clones it.
func BenchmarkCloneSparse(b *testing.B) {
	extents := map[int64]int{}
	for off := int64(0); off < 2560<<20; off += 64 << 20 {
		extents[off] = 32 << 20 // half of each 64 MiB is data: ~1.25 GiB written
	}

	src, _ := sparseImage(b, 4608<<20, extents)

	for b.Loop() {
		dst := filepath.Join(b.TempDir(), "dst")

		mode, err := CloneFile(src, dst)
		if err != nil {
			b.Fatal(err)
		}

		b.ReportMetric(float64(allocatedBytes(b, dst))/(1<<20), "MiB-allocated")
		b.Logf("mode %s", mode)
	}
}
