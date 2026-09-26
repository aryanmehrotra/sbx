//go:build !windows

package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// A jail's links to the VM's own drives are the same blocks, counted once under Disks; so is a
// link to the shared kernel, which is the artifact cache's. What only the jail names - a per-VM
// kernel copy, a link that outlived a replaced snapshot - is disk nothing else counts, and
// `sbx doctor` must see it.
func TestFirecrackerDiskUsageCountsWhatOnlyAJailHolds(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SBX_FC_STATE", root)

	const k = 64 << 10

	write := func(p string, n int) string {
		t.Helper()
		must := func(err error) {
			if err != nil {
				t.Fatal(err)
			}
		}
		must(os.MkdirAll(filepath.Dir(p), 0o700))
		must(os.WriteFile(p, make([]byte, n), 0o600))

		return p
	}

	dir := filepath.Join(root, "vms", "a")
	jail := filepath.Join(dir, fc.JailDirName, "firecracker", "sbx-0", "root")

	rootfs := write(filepath.Join(dir, fc.RootfsName), 8*k)
	kernel := write(filepath.Join(root, "artifacts", "vmlinux"), 16*k)
	copyK := write(filepath.Join(jail, "vmlinux"), 16*k)          // a per-VM copy
	stale := write(filepath.Join(jail, fc.StateName+".new"), 4*k) // only the jail names it

	for _, l := range [][2]string{{rootfs, fc.RootfsName}, {kernel, "shared-vmlinux"}} {
		if err := os.Link(l[0], filepath.Join(jail, l[1])); err != nil {
			t.Fatal(err)
		}
	}

	u, err := FirecrackerDiskUsage()
	if err != nil {
		t.Fatal(err)
	}

	if want := allocated(copyK) + allocated(stale); u.Jails != want {
		t.Fatalf("Jails = %d, want %d: the copy and the stale file, and neither link", u.Jails, want)
	}

	if u.Total() != u.Memory+u.Disks+u.Snapshots+u.Volumes+u.Jails {
		t.Fatalf("Total %d leaves the jails out: %+v", u.Total(), u)
	}
}
