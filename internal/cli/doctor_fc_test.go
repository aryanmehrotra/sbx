package cli

import (
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// The microVM row is fchost's; these are only the rows under it, and only for a host that runs
// Firecracker itself. There is no second "firecracker" row any more: it graded every M3+ Mac ✗
// from a decision of its own while the microVM row, the provider and the redirect said helper-vm.
func TestDoctorFirecrackerDetailRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  hostcap.Backend
		mkfs  string
		fwd   string
		names []string
	}{
		{"direct", hostcap.Direct, "/sbin/mkfs.ext4", "1\n", []string{"mkfs.ext4", "vm bridges isolated"}},
		{"direct, no e2fsprogs", hostcap.Direct, "", "0", []string{"mkfs.ext4", "vm bridges isolated"}},
		{"helper vm", hostcap.HelperVM, "", "", nil},
		{"refused", hostcap.Refused, "/sbin/mkfs.ext4", "0", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := firecrackerCapabilities(tc.kind, tc.mkfs, fc.CheckBridgeIsolation(tc.fwd, acceptAll), nil)

			var names []string
			for _, c := range caps {
				names = append(names, c.Name)

				if c.Name == "firecracker" {
					t.Errorf("a second firecracker row came back: %+v", c)
				}

				if !c.Have && c.Meaning == "" {
					t.Errorf("%s absent with no meaning", c.Name)
				}
			}

			if strings.Join(names, ",") != strings.Join(tc.names, ",") {
				t.Fatalf("rows = %v, want %v", names, tc.names)
			}
		})
	}

	caps := firecrackerCapabilities(hostcap.Direct, "", fc.CheckBridgeIsolation("0", nil), nil)
	if caps[0].Have || !strings.Contains(caps[0].Meaning, "e2fsprogs") {
		t.Fatalf("mkfs row = %+v", caps[0])
	}
}

// A Direct host's doctor says what its sleeping VMs hold on disk, broken down.
func TestDoctorShowsMicroVMDiskUsage(t *testing.T) {
	u := &provider.FirecrackerUsage{Root: "/root/.sbx/fc", VMs: 3, Memory: 768 << 20, Disks: 2 << 30, Snapshots: 256 << 20}

	caps := firecrackerCapabilities(hostcap.Direct, "/sbin/mkfs.ext4", fc.CheckBridgeIsolation("0", nil), u)

	last := caps[len(caps)-1]
	if last.Name != "microVM disk" || !last.Have ||
		!strings.Contains(last.Detail, "3.0 GiB in 3 VMs under /root/.sbx/fc (memory 768.0 MiB, disks 2.0 GiB, snapshots 256.0 MiB, volumes 0 KiB)") {
		t.Fatalf("disk row = %+v", last)
	}

	u.PoolMembers, u.PoolMemory = 2, 512<<20

	caps = firecrackerCapabilities(hostcap.Direct, "/sbin/mkfs.ext4", fc.CheckBridgeIsolation("0", nil), u)
	if last := caps[len(caps)-1]; !strings.Contains(last.Detail, "; 2 parked warm-pool members hold 512.0 MiB of it") {
		t.Fatalf("disk row with a pool = %+v", last)
	}

	if caps := firecrackerCapabilities(hostcap.HelperVM, "", fc.BridgeIsolation{}, u); len(caps) != 0 {
		t.Fatalf("a helper-VM host graded its own disk: %+v", caps)
	}
}

func acceptAll() (string, error) { return "-P FORWARD ACCEPT\n", nil }

// ip_forward=1 with an ACCEPT policy is a host that routes between sandbox bridges: graded ✗
// with what to do, not passed because forwarding is merely on.
func TestDoctorGradesTheForwardPolicy(t *testing.T) {
	caps := firecrackerCapabilities(hostcap.Direct, "/sbin/mkfs.ext4", fc.CheckBridgeIsolation("1", acceptAll), nil)
	if row := caps[1]; row.Name != "vm bridges isolated" || row.Have || !strings.Contains(row.Meaning, "FORWARD DROP") {
		t.Fatalf("row = %+v", row)
	}
}
