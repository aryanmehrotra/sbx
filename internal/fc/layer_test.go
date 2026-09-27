package fc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The base a layered VM boots is its image's file itself - the same inode, so nothing is copied -
// and it is held to what a jail may be given by link (sharable): the builder writes it 0600, and a
// 0600 base would be copied into every jail by Stage.Shared, which is the copy this removes.
func TestLinkSharedLinksTheImageAndHoldsItReadOnly(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "rootfs.ext4"), filepath.Join(dir, "vm", BaseName)

	if err := os.WriteFile(src, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}

	how, err := LinkShared(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	a, _ := os.Stat(src)
	b, _ := os.Stat(dst)

	if how != CloneLink || a == nil || b == nil || !os.SameFile(a, b) {
		t.Fatalf("LinkShared = %q: the base is not the image's own file", how)
	}

	if !sharable(b) {
		t.Fatalf("the base is %v: a jail would be given a copy of it, not the file", b.Mode())
	}

	// Again, over itself (a retried create): the image must survive it.
	if _, err := LinkShared(src, dst); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(src); string(got) != "image" {
		t.Fatalf("linking over an existing link emptied the image: %q", got)
	}
}

// A symlink is not a file sbx built, and link(2) would link the symlink itself.
func TestLinkSharedRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "rootfs.ext4")

	if err := os.WriteFile(real, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if _, err := LinkShared(link, filepath.Join(dir, BaseName)); err == nil {
		t.Fatal("a symlinked base was linked")
	}
}

type sizeRecorder struct {
	size  int64
	label string
	empty bool
}

func (*sizeRecorder) TakesTar(context.Context) bool { return true }

func (r *sizeRecorder) Build(_ context.Context, src, img, label string) error {
	st, err := os.Stat(img)
	if err != nil {
		return err
	}

	ents, _ := os.ReadDir(src)
	r.size, r.label, r.empty = st.Size(), label, len(ents) == 0

	return nil
}

// The writable layer is an empty ext4 of exactly the size asked: that size is the most one VM can
// write to its root filesystem on the host's disk.
func TestBuildUpperDriveIsAnEmptyExt4OfTheSize(t *testing.T) {
	rec := &sizeRecorder{}
	dst := filepath.Join(t.TempDir(), UpperName)

	if err := BuildUpperDrive(t.Context(), rec, dst, 3<<30); err != nil {
		t.Fatal(err)
	}

	if rec.size != 3<<30 || !rec.empty || rec.label != "sbxupper" {
		t.Fatalf("built %+v, want an empty 3 GiB sbxupper", rec)
	}
}
