package cli

import (
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
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
			caps := firecrackerCapabilities(tc.kind, tc.mkfs, tc.fwd)

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

	caps := firecrackerCapabilities(hostcap.Direct, "", "0")
	if caps[0].Have || !strings.Contains(caps[0].Meaning, "e2fsprogs") {
		t.Fatalf("mkfs row = %+v", caps[0])
	}
}
