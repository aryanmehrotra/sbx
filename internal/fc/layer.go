package fc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// A layered VM (v0.13) does not copy its image. Its root drive is its image's root filesystem
// itself - one file per image, built once into the rootfs cache, root's and read-only - linked into
// the VM's directory and attached read-only; what the guest writes goes to a small drive of the
// VM's own that `sbx fc-init` lays over it with overlayfs. Before, every VM cloned the whole image:
// instant with reflink (XFS, btrfs), and a full copy of its data without it - about 16 s for the
// 7.4 GiB code-interpreter image on a CI runner's ext4.
//
// The base is shared the way the kernel is (Stage.Shared): one inode for every VM of the image,
// never given to a VM's uid, and held root's and read-only - a VMM that could write it would
// change every other VM's root filesystem.

// CloneLink is how a layered VM's base came to be in its directory: a hard link to the image's
// file, so nothing was read or written.
const CloneLink = "link"

// LinkShared puts the shared, read-only file src at dst: a hard link where src's filesystem
// allows it (the rootfs cache and the VM directories are both under the state directory), a
// clone of it otherwise - in both cases held root's and read-only (sharable), which it must be
// before a jail may be given it by link. It says which it did.
func LinkShared(src, dst string) (string, error) {
	if err := holdShared(src); err != nil {
		return "", err
	}

	// Never into a file already at dst: CloneFile truncates what it writes to, and a name that
	// already links the base would take the base with it.
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.Link(src, dst); err == nil {
		return CloneLink, nil
	}

	how, err := CloneFile(src, dst)
	if err != nil {
		return "", err
	}

	return how, holdShared(dst)
}

// holdShared makes path what a file every VM shares must be: a plain file owned by the one running
// sbx, readable by anyone and writable by nobody. A file someone else owns is refused rather than
// chmod'd: sbx made the cache, and a base that is not its own is not one to hand every VM.
func holdShared(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if !st.Mode().IsRegular() {
		return fmt.Errorf("the shared root filesystem %s is a %s, not a file sbx built", path, st.Mode().Type())
	}

	if sharable(st) {
		return nil
	}

	if uid, known := fileOwner(st); known && uid != geteuid() {
		return fmt.Errorf("the shared root filesystem %s is owned by uid %d, not by sbx (uid %d); it is "+
			"every VM's root and must be sbx's own - remove it and it is rebuilt", path, uid, geteuid())
	}

	return os.Chmod(path, 0o444)
}

// BuildUpperDrive makes a layered VM's writable layer at dst: an empty ext4 of exactly size bytes,
// sparse, so it costs what the guest writes and can never grow past size. That is the bound on
// what one VM can write to its root filesystem on the host's disk.
func BuildUpperDrive(ctx context.Context, ext4 Ext4Builder, dst string, size int64) error {
	empty, err := os.MkdirTemp(filepath.Dir(dst), ".upper-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(empty)

	if err := sparseFile(dst, size); err != nil {
		return err
	}

	if err := ext4.Build(ctx, empty, dst, "sbxupper"); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("making the VM's writable layer: %w", err)
	}

	return nil
}
