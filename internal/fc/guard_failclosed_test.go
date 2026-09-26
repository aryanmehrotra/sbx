package fc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// A host guard that cannot be installed or verified refuses the tap: the VM does not start, and
// the host is left as it was - no bridge up without its guard. v0.12 warned and booted anyway,
// leaving every host service bound to 0.0.0.0 reachable from the guest.
func TestAGuardThatCannotBeInstalledRefusesTheVM(t *testing.T) {
	for name, tables := range map[string]*fakeTables{
		"no iptables":         func() *fakeTables { f := newFakeTables(); f.absent = true; return f }(),
		"mangle refused":      func() *fakeTables { f := newFakeTables(); f.fail = "-t mangle -N"; return f }(),
		"jump not insertable": func() *fakeTables { f := newFakeTables(); f.fail = "-I INPUT"; return f }(),
	} {
		t.Run(name, func(t *testing.T) {
			ip := &fakeIP{links: map[string]bool{}}
			n := &IPNetwork{Owner: -1, Guard: tables.guard(), Run: ip.run}

			err := n.EnsureTap(context.Background(), Addr{Slot: 5})
			if err == nil {
				t.Fatal("a VM started on a bridge whose host guard could not be installed")
			}

			for _, want := range []string{"sbxfc5", "10.231.5.1", FirewallEnv + "=unmanaged"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal does not say %q: %v", want, err)
				}
			}

			if name == "no iptables" && !errors.Is(err, ErrNoFirewall) {
				t.Fatalf("not ErrNoFirewall: %v", err)
			}

			if ip.links["sbxfc5"] || ip.links["sbxfc5-0"] {
				t.Fatalf("an unguarded bridge or tap was left: %v", ip.cmds)
			}

			if slices.Contains(ip.cmds, "link set sbxfc5 up") {
				t.Fatalf("the bridge was brought up before its guard: %v", ip.cmds)
			}
		})
	}
}

// A wake of a bridge whose guard something removed, and which cannot be put back, is refused too.
func TestAWakeWhoseGuardCannotBePutBackIsRefused(t *testing.T) {
	ip := &fakeIP{links: map[string]bool{}}
	tables := newFakeTables()
	n := &IPNetwork{Owner: -1, Guard: tables.guard(), Run: ip.run}

	if err := n.EnsureTap(context.Background(), Addr{Slot: 5}); err != nil {
		t.Fatal(err)
	}

	tables.chains["INPUT"] = []string{"-i eth0 -j ACCEPT"} // a firewall reload
	tables.fail = "-I INPUT"

	err := n.EnsureTap(context.Background(), Addr{Slot: 5, Index: 1})
	if err == nil || !strings.Contains(err.Error(), "sbxfc5") {
		t.Fatalf("a wake on an unguarded bridge: %v", err)
	}

	if ip.links["sbxfc5-1"] {
		t.Fatal("the refused VM's tap was made anyway")
	}

	// And a host whose iptables went away since the bridge was made.
	tables.fail, tables.absent = "", true
	if err := n.EnsureTap(context.Background(), Addr{Slot: 5, Index: 2}); !errors.Is(err, ErrNoFirewall) {
		t.Fatalf("a wake with no iptables: %v", err)
	}
}

// IPv6 left on is the guard with a hole in it: a guest reaches host services on [::] over the
// bridge's link-local address, which no IPv4 rule sees.
func TestABridgeWhoseIPv6CannotBeTurnedOffIsRefused(t *testing.T) {
	ip := &fakeIP{links: map[string]bool{}}
	g := newFakeTables().guard()
	g.Sysctl = func(string, string) error { return errors.New("read-only file system") }

	n := &IPNetwork{Owner: -1, Guard: g, Run: ip.run}
	if err := n.EnsureTap(context.Background(), Addr{Slot: 3}); err == nil || !strings.Contains(err.Error(), "IPv6") {
		t.Fatalf("err = %v", err)
	}

	if ip.links["sbxfc3"] {
		t.Fatal("the bridge was left")
	}
}

// Unmanaged: the operator's own firewall owns the host, and sbx writes nothing to it.
func TestAnUnmanagedFirewallWritesNoRule(t *testing.T) {
	ip := &fakeIP{links: map[string]bool{}}
	n := &IPNetwork{Owner: -1, Run: ip.run} // Guard nil: FirewallUnmanaged

	if err := n.EnsureTap(context.Background(), Addr{Slot: 4}); err != nil {
		t.Fatal(err)
	}
}

func TestFirewallModeFromEnv(t *testing.T) {
	for v, want := range map[string]FirewallMode{"": FirewallManaged, "managed": FirewallManaged, "unmanaged": FirewallUnmanaged, "UNMANAGED": FirewallUnmanaged} {
		if got, err := FirewallFromEnv(func(string) string { return v }); err != nil || got != want {
			t.Fatalf("%q -> %v, %v", v, got, err)
		}
	}

	if _, err := FirewallFromEnv(func(string) string { return "off" }); err == nil {
		t.Fatal("an unknown mode was read as one of the two")
	}
}

// A jailed VMM opens its tap as its own uid, so the tap is made for that uid; one left by an
// unjailed run (root's) is made again, since a tun device's owner cannot be changed.
func TestATapBelongsToItsVMsUID(t *testing.T) {
	ip := &fakeIP{links: map[string]bool{}}
	owners := map[string]int{}
	n := &IPNetwork{Owner: -1, Run: ip.run,
		OwnerOf:  func(a Addr) int { return 900000 + a.Slot*256 + a.Index },
		TapOwner: func(tap string) (int, bool) { o, ok := owners[tap]; return o, ok },
	}

	if err := n.EnsureTap(context.Background(), Addr{Slot: 2, Index: 3}); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(ip.cmds, "tuntap add dev sbxfc2-3 mode tap user 900515") {
		t.Fatalf("tap not made for the VM's uid: %v", ip.cmds)
	}

	owners["sbxfc2-3"] = 0
	ip.cmds = nil

	if err := n.EnsureTap(context.Background(), Addr{Slot: 2, Index: 3}); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(ip.cmds, "link del sbxfc2-3") || !slices.Contains(ip.cmds, "tuntap add dev sbxfc2-3 mode tap user 900515") {
		t.Fatalf("root's tap was kept for a jailed VMM: %v", ip.cmds)
	}

	owners["sbxfc2-3"] = 900515
	ip.cmds = nil

	if err := n.EnsureTap(context.Background(), Addr{Slot: 2, Index: 3}); err != nil {
		t.Fatal(err)
	}

	if slices.Contains(ip.cmds, "link del sbxfc2-3") {
		t.Fatalf("a tap already the VM's was made again: %v", ip.cmds)
	}
}
