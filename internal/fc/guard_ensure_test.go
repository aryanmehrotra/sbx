package fc

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// writes are the commands that change a table, as opposed to -C, -L and -N-that-fails checks.
func writes(cmds []string) []string {
	var out []string

	for _, c := range cmds {
		c = strings.TrimPrefix(c, "-t mangle ")
		if !strings.HasPrefix(c, "-C") && !strings.HasPrefix(c, "-n -L") && !strings.HasPrefix(c, "-N") {
			out = append(out, c)
		}
	}

	return out
}

// A rule flushed while the bridge stood - a firewall reload, `iptables -F`, `iptables -t mangle
// -F` - was gone until the sandbox was recreated, because the guard was installed only when the
// bridge was made. The next wake now puts it back, and a wake of a whole guard writes nothing.
func TestAWakePutsBackAGuardSomethingFlushed(t *testing.T) {
	ip := &fakeIP{links: map[string]bool{}}
	tables := newFakeTables()

	var warned []string

	n := &IPNetwork{Owner: -1, Guard: tables.guard(), Run: ip.run,
		Warn: func(s string) { warned = append(warned, s) }}

	a := Addr{Slot: 5}
	if err := n.EnsureTap(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	want := map[string][]string{}
	for k, v := range tables.chains {
		want[k] = slices.Clone(v)
	}

	// An ordinary wake: checks only.
	before := len(tables.cmds)
	if err := n.EnsureTap(context.Background(), Addr{Slot: 5, Index: 1}); err != nil {
		t.Fatal(err)
	}

	if w := writes(tables.cmds[before:]); len(w) > 0 || len(warned) > 0 {
		t.Fatalf("a wake of a whole guard wrote %q, warned %q", w, warned)
	}

	for _, flush := range []func(){
		func() { tables.chains["INPUT"] = []string{"-i eth0 -j ACCEPT", "-i docker0 -j DOCKER-IN"} },
		func() {
			tables.chains["mangle/PREROUTING"], tables.chains["mangle/SBX-FC5"] = []string{"-j CNI-MARK"}, nil
		},
		func() { tables.chains["mangle/FORWARD"] = nil },
		func() { tables.chains["SBX-FC5"] = nil },
	} {
		flush()

		warned = nil
		if err := n.EnsureTap(context.Background(), a); err != nil {
			t.Fatal(err)
		}

		for k, v := range want {
			got := tables.chains[k]
			slices.Sort(got)
			w := slices.Clone(v)
			slices.Sort(w)

			if !slices.Equal(got, w) {
				t.Fatalf("after a flush and a wake, %s = %q, want %q", k, tables.chains[k], v)
			}
		}

		if len(warned) != 1 || !strings.Contains(warned[0], "put back") {
			t.Fatalf("a repair was not reported: %q", warned)
		}
	}
}

// The daemon's reconcile does the same for a bridge nothing is waking, and nothing for a slot
// with no bridge.
func TestEnsureGuardRepairsAStandingBridgeAndIgnoresAMissingOne(t *testing.T) {
	ip := &fakeIP{links: map[string]bool{}}
	tables := newFakeTables()
	n := &IPNetwork{Owner: -1, Guard: tables.guard(), Run: ip.run}

	if err := n.EnsureTap(context.Background(), Addr{Slot: 5}); err != nil {
		t.Fatal(err)
	}

	tables.chains["mangle/PREROUTING"] = []string{"-j CNI-MARK"}

	n.EnsureGuard(context.Background(), 5)

	if !slices.Contains(tables.chains["mangle/PREROUTING"], "-i sbxfc5 -j SBX-FC5") {
		t.Fatalf("reconcile did not put the hook back: %q", tables.chains["mangle/PREROUTING"])
	}

	before := len(tables.cmds)
	n.EnsureGuard(context.Background(), 9)

	if len(tables.cmds) != before {
		t.Fatalf("a slot with no bridge ran iptables: %q", tables.cmds[before:])
	}
}

// Repairing a missing hook does not flush a chain the other hooks still jump to: an empty chain
// is an open bridge for as long as it stays empty.
func TestARepairLeavesAWholeChainAlone(t *testing.T) {
	tables := newFakeTables()
	g := tables.guard()
	a := Addr{Slot: 5}

	if err := g.Install(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	tables.chains["mangle/FORWARD"] = nil
	before := len(tables.cmds)

	if repaired, err := g.Ensure(context.Background(), a); err != nil || !repaired {
		t.Fatalf("Ensure = %v, %v", repaired, err)
	}

	for _, c := range tables.cmds[before:] {
		if strings.Contains(c, "-F SBX-FC5") {
			t.Fatalf("a whole chain was flushed to repair a hook: %q", tables.cmds[before:])
		}
	}
}
