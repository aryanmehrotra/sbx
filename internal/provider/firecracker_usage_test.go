package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

func TestFirecrackerDiskUsageCountsMemoryDisksAndSnapshots(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SBX_FC_STATE", root)

	write := func(p string, n int) {
		t.Helper()

		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	const k = 64 << 10

	write(filepath.Join(root, "vms", "a", fc.MemName), 4*k)
	write(filepath.Join(root, "vms", "a", fc.StateName), k)
	write(filepath.Join(root, "vms", "a", fc.RootfsName), 8*k)
	write(filepath.Join(root, "vms", "b", fc.MemName), 4*k)
	write(filepath.Join(root, "snapshots", "img-1", fc.MemName), 2*k)

	u, err := FirecrackerDiskUsage()
	if err != nil {
		t.Fatal(err)
	}

	if u.VMs != 2 || u.Memory < 9*k || u.Disks < 8*k || u.Snapshots < 2*k || u.Total() != u.Memory+u.Disks+u.Snapshots {
		t.Fatalf("usage = %+v", u)
	}

	t.Setenv("SBX_FC_STATE", filepath.Join(root, "never-made"))

	if u, err := FirecrackerDiskUsage(); err != nil || u.Total() != 0 {
		t.Fatalf("no state directory = %+v, %v", u, err)
	}
}
