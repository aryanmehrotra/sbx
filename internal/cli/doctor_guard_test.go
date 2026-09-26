package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A Direct host's doctor says whether iptables is there at all, and how many of its VM bridges
// have their guard in place - the one thing that says the host is actually closed to its guests.
func TestDoctorGradesIptablesAndTheBridgeGuards(t *testing.T) {
	rows := func(ipt error, c fc.GuardCount, cerr error) map[string]Capability {
		out := map[string]Capability{}
		for _, r := range firecrackerGuardRows(hostcap.Direct, ipt, c, cerr) {
			out[r.Name] = r
		}

		return out
	}

	all := rows(nil, fc.GuardCount{Guarded: 2, Total: 2}, nil)
	if r := all["iptables"]; !r.Have {
		t.Fatalf("iptables row = %+v", r)
	}

	if r := all["vm bridges guarded"]; !r.Have || !strings.Contains(r.Detail, "2/2") {
		t.Fatalf("guarded row = %+v", r)
	}

	some := rows(nil, fc.GuardCount{Guarded: 1, Total: 2, Unguarded: []string{"sbxfc5"}}, nil)
	if r := some["vm bridges guarded"]; r.Have || !strings.Contains(r.Detail, "1/2") ||
		!strings.Contains(r.Detail, "sbxfc5") || r.Meaning == "" {
		t.Fatalf("a bridge without its guard = %+v", r)
	}

	none := rows(fc.ErrNoFirewall, fc.GuardCount{}, fc.ErrNoFirewall)
	if r := none["iptables"]; r.Have || !strings.Contains(r.Meaning, "10.231") {
		t.Fatalf("no iptables = %+v", r)
	}

	if r := rows(nil, fc.GuardCount{Total: 1}, errors.New("Permission denied (you must be root)"))["vm bridges guarded"]; r.Have || !strings.Contains(r.Meaning, "sudo") {
		t.Fatalf("an unreadable table = %+v", r)
	}

	if len(firecrackerGuardRows(hostcap.HelperVM, nil, fc.GuardCount{}, nil)) != 0 {
		t.Fatal("a helper-VM host graded its own iptables")
	}
}

// A pvc is an ext4 image under volumes/, and a fleet of them is disk too.
func TestDoctorCountsMicroVMVolumes(t *testing.T) {
	u := &provider.FirecrackerUsage{Root: "/r", VMs: 1, Memory: 1 << 20, Disks: 1 << 20, Volumes: 3 << 30}

	caps := firecrackerCapabilities(hostcap.Direct, "/sbin/mkfs.ext4", fc.CheckBridgeIsolation("0", nil), u)
	if last := caps[len(caps)-1]; !strings.Contains(last.Detail, "volumes 3.0 GiB") || !strings.HasPrefix(last.Detail, "3.0 GiB") {
		t.Fatalf("disk row = %+v", last)
	}
}
