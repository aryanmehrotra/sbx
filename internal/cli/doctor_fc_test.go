package cli

import (
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
)

func TestDoctorSaysWhyFirecrackerCannotRun(t *testing.T) {
	kvm := hostcap.KVM{Present: true, Usable: true, APIVersion: 12}

	for _, tc := range []struct {
		name  string
		r     hostcap.Report
		fwd   string
		have  bool
		says  string
		names []string
	}{
		{"linux direct", hostcap.Report{OS: "linux", Arch: "arm64", KVM: kvm, Mkfs: "/sbin/mkfs.ext4"}, "1\n",
			true, "direct", []string{"firecracker", "mkfs.ext4", "vm bridges isolated"}},
		{"linux, no e2fsprogs", hostcap.Report{OS: "linux", Arch: "arm64", KVM: kvm}, "0",
			true, "direct", []string{"firecracker", "mkfs.ext4", "vm bridges isolated"}},
		{"linux, no kvm", hostcap.Report{OS: "linux", Arch: "amd64"}, "0",
			false, "modprobe", []string{"firecracker", "mkfs.ext4", "vm bridges isolated"}},
		{"mac that nests", hostcap.Report{OS: "darwin", Arch: "arm64", CPUBrand: "Apple M4", OSVersion: "26.4.1", Nested: true, NestedHint: "Apple M4"}, "",
			false, "helper VM", []string{"firecracker"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := firecrackerCapabilities(tc.r, tc.fwd)

			var names []string
			for _, c := range caps {
				names = append(names, c.Name)

				if !c.Have && c.Meaning == "" {
					t.Errorf("%s absent with no meaning", c.Name)
				}
			}

			if strings.Join(names, ",") != strings.Join(tc.names, ",") {
				t.Fatalf("rows = %v, want %v", names, tc.names)
			}

			if caps[0].Have != tc.have || !strings.Contains(caps[0].Detail+caps[0].Meaning, tc.says) {
				t.Fatalf("firecracker row = %+v", caps[0])
			}
		})
	}

	caps := firecrackerCapabilities(hostcap.Report{OS: "linux", Arch: "arm64", KVM: kvm}, "0")
	if caps[1].Have || !strings.Contains(caps[1].Meaning, "e2fsprogs") {
		t.Fatalf("mkfs row = %+v", caps[1])
	}
}
