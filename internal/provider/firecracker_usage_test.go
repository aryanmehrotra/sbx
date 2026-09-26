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

// Parked warm-pool members are counted apart: each holds a memory file as big as its RAM, which is
// the whole price of a pool that waits asleep.
func TestFirecrackerDiskUsageCountsParkedPoolMembers(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SBX_FC_STATE", root)

	const k = 64 << 10

	for name, pooled := range map[string]bool{"m1": true, "m2": true, "claimed": false} {
		dir := filepath.Join(root, "vms", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}

		rec := `{"pooled":false}`
		if pooled {
			rec = `{"pooled":true}`
		}

		if err := os.WriteFile(filepath.Join(dir, "vm.json"), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, fc.MemName), make([]byte, 4*k), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	u, err := FirecrackerDiskUsage()
	if err != nil {
		t.Fatal(err)
	}

	if u.VMs != 3 || u.PoolMembers != 2 || u.PoolMemory < 8*k || u.PoolMemory >= u.Memory {
		t.Fatalf("usage = %+v, want 2 parked members holding two of the three memory files", u)
	}
}
