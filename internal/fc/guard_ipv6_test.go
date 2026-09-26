package fc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sysctlRoot is a fake /proc/sys with IPv6 in the kernel and sbxfc<slot>'s disable_ipv6 at v.
func sysctlRoot(t *testing.T, slot int, v string) (root, file string) {
	t.Helper()

	root = t.TempDir()
	file = filepath.Join(root, "net", "ipv6", "conf", Addr{Slot: slot}.Bridge(), "disable_ipv6")
	must(t, os.MkdirAll(filepath.Dir(file), 0o755))
	must(t, os.WriteFile(file, []byte(v+"\n"), 0o644))

	return root, file
}

// realSysctl writes the file, as NewGuard's does.
func realSysctl(p, v string) error { return os.WriteFile(p, []byte(v), 0o644) }

// The IPv6 half of the guard is checked and put back like the iptables half: a bridge whose
// disable_ipv6 went back to 0 (a sysctl reload, a NetworkManager profile, a hand) has guests that
// reach every [::] service on the host over link-local, whatever the iptables rules say.
func TestEnsurePutsBackIPv6OffOnAStandingBridge(t *testing.T) {
	a := Addr{Slot: 5}
	root, file := sysctlRoot(t, a.Slot, "0")

	g := newFakeTables().guard()
	g.ProcSys, g.Sysctl = root, realSysctl

	must(t, g.Install(context.Background(), a))

	if whole, err := g.Whole(context.Background(), a); err != nil || whole {
		t.Fatalf("Whole with IPv6 on = %v, %v; want not whole", whole, err)
	}

	repaired, err := g.Ensure(context.Background(), a)
	if err != nil || !repaired {
		t.Fatalf("Ensure = %v, %v; want repaired", repaired, err)
	}

	if b, _ := os.ReadFile(file); strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("disable_ipv6 = %q after Ensure", b)
	}

	if whole, err := g.Whole(context.Background(), a); err != nil || !whole {
		t.Fatalf("Whole after the repair = %v, %v", whole, err)
	}
}

// One that cannot be turned off again is the VM's refusal - fail closed - on a wake (EnsureTap on
// a standing bridge), not a VM started with the host open to it.
func TestAStandingBridgeWhoseIPv6CannotBeTurnedOffRefusesTheVM(t *testing.T) {
	a := Addr{Slot: 5}
	root, _ := sysctlRoot(t, a.Slot, "0")

	g := newFakeTables().guard()
	g.ProcSys = root
	g.Sysctl = func(string, string) error { return errors.New("read-only file system") }

	must(t, g.Install(context.Background(), a))

	ip := &fakeIP{links: map[string]bool{a.Bridge(): true}}
	n := &IPNetwork{Owner: -1, Guard: g, Run: ip.run}

	err := n.EnsureTap(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("EnsureTap on a bridge with IPv6 on = %v, want a refusal", err)
	}

	if ip.links[a.Tap()] {
		t.Fatal("the VM's tap was made on a bridge its guests could leave over IPv6")
	}

	// A write that "succeeds" but does not take (disable_ipv6 still 0) is the same refusal.
	g.Sysctl = func(string, string) error { return nil }
	if err := n.EnsureTap(context.Background(), a); err == nil {
		t.Fatal("a write that did not take was trusted")
	}
}

// A kernel without IPv6 has nothing to turn off: whole, and nothing written.
func TestAKernelWithoutIPv6IsWhole(t *testing.T) {
	a := Addr{Slot: 5}

	g := newFakeTables().guard()
	g.ProcSys = t.TempDir() // no net/ipv6 at all
	g.Sysctl = func(p, _ string) error { t.Fatalf("wrote %s on a kernel without IPv6", p); return nil }

	must(t, g.Install(context.Background(), a))

	if repaired, err := g.Ensure(context.Background(), a); err != nil || repaired {
		t.Fatalf("Ensure = %v, %v", repaired, err)
	}
}

// `sbx doctor` names a bridge whose IPv6 is on, as the reason it is not guarded.
func TestCountGuardsNamesABridgeWithIPv6On(t *testing.T) {
	a := Addr{Slot: 5}
	root, _ := sysctlRoot(t, a.Slot, "0")

	g := newFakeTables().guard()
	g.ProcSys, g.Sysctl = root, realSysctl

	must(t, g.Install(context.Background(), a))

	c, err := CountGuards(context.Background(), g, []int{a.Slot})
	if err != nil || c.Guarded != 0 || len(c.IPv6On) != 1 || c.IPv6On[0] != a.Bridge() {
		t.Fatalf("CountGuards = %+v, %v", c, err)
	}
}

// RecheckGuard is the wake's recheck for a VM resumed in place: a standing bridge's guard is put
// back or the resume refused, and its tap is never touched (the paused VMM holds it).
func TestRecheckGuardRepairsOrRefusesAndLeavesTheTapAlone(t *testing.T) {
	a := Addr{Slot: 5}
	tables := newFakeTables()
	g := tables.guard()

	ip := &fakeIP{links: map[string]bool{a.Bridge(): true, a.Tap(): true}}
	n := &IPNetwork{Owner: -1, Guard: g, Run: ip.run}

	if err := n.RecheckGuard(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	if whole, _ := g.Whole(context.Background(), a); !whole {
		t.Fatal("RecheckGuard did not put the guard on a standing bridge")
	}

	for _, c := range ip.cmds {
		if strings.Contains(c, a.Tap()) {
			t.Fatalf("RecheckGuard touched the paused VMM's tap: %q", c)
		}
	}

	root, _ := sysctlRoot(t, a.Slot, "0")
	g.ProcSys = root
	g.Sysctl = func(string, string) error { return errors.New("read-only file system") }

	if err := n.RecheckGuard(context.Background(), a); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("RecheckGuard with IPv6 stuck on = %v, want a refusal", err)
	}

	// No bridge: nothing a guest could reach the host through.
	delete(ip.links, a.Bridge())
	if err := n.RecheckGuard(context.Background(), a); err != nil {
		t.Fatalf("RecheckGuard with no bridge = %v", err)
	}
}
