package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// A layered VM's base.ext4 is a hard link to its image's one root filesystem, and so is every other
// VM's of that image and every layered snapshot's. It is counted once, as the images the VMs
// share (Bases) - not once per VM as their disks, which would report N copies that do not exist.
// Each VM's own writable layer is its disk.
func TestFirecrackerDiskUsageCountsASharedBaseOnce(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SBX_FC_STATE", root)

	const k = 64 << 10

	img := filepath.Join(root, "rootfs", "abc", fc.RootfsName)
	if err := os.MkdirAll(filepath.Dir(img), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(img, make([]byte, 16*k), 0o444); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{"vms/a", "vms/b", "snapshots/s1"} {
		dir := filepath.Join(root, d)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.Link(img, filepath.Join(dir, fc.BaseName)); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, fc.UpperName), make([]byte, k), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	u, err := FirecrackerDiskUsage()
	if err != nil {
		t.Fatal(err)
	}

	base, upper := allocated(img), allocated(filepath.Join(root, "vms", "a", fc.UpperName))

	if u.Bases != base {
		t.Fatalf("Bases = %d, want the one image's %d", u.Bases, base)
	}

	if u.Disks != 2*upper {
		t.Fatalf("Disks = %d, want the two writable layers' %d: a shared base was counted as a VM's disk", u.Disks, 2*upper)
	}

	if u.Snapshots != upper {
		t.Fatalf("Snapshots = %d, want the snapshot's own layer %d, not the base again", u.Snapshots, upper)
	}

	if u.Total() != u.Memory+u.Disks+u.Bases+u.Snapshots+u.Volumes+u.Jails {
		t.Fatalf("Total leaves out a part: %+v", u)
	}
}
