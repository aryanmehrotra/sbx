package fc

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// fakeTables is iptables' filter table as far as Guard can see it: named chains of rules, INPUT
// among them. It interprets the commands rather than recording them, so a test asserts on the
// state they leave - which is what "idempotent" and "never half a chain" are claims about.
type fakeTables struct {
	chains map[string][]string
	cmds   []string
	fail   string // a command prefix that fails
	absent bool   // no iptables at all
}

func newFakeTables() *fakeTables {
	// Somebody else's rules, which nothing sbx does may touch.
	return &fakeTables{chains: map[string][]string{
		"INPUT":             {"-i eth0 -j ACCEPT", "-i docker0 -j DOCKER-IN"},
		"OTHERS":            {"-j RETURN"},
		"mangle/PREROUTING": {"-j CNI-MARK"},
		"mangle/FORWARD":    {},
	}}
}

func (f *fakeTables) run(_ context.Context, args ...string) (string, error) {
	if f.absent {
		return "", ErrNoFirewall
	}

	cmd := strings.Join(args, " ")
	f.cmds = append(f.cmds, cmd)

	if f.fail != "" && strings.HasPrefix(cmd, f.fail) {
		return "", errors.New("iptables: No chain/target/match by that name")
	}

	// Chains are keyed "<table>/<chain>", the filter table's by bare name.
	table := ""
	if args[0] == "-t" {
		table, args = args[1]+"/", args[2:]
	}

	if args[0] == "-n" { // -n -L chain
		args = args[1:]
	}

	op, chain, rule := args[0], table+args[1], strings.Join(args[2:], " ")
	rules, exists := f.chains[chain]
	missing := errors.New("iptables: No chain/target/match by that name")

	switch op {
	case "-N":
		if exists {
			return "", errors.New("iptables: Chain already exists")
		}

		f.chains[chain] = []string{}
	case "-L":
		if !exists {
			return "", missing
		}
	case "-F":
		if !exists {
			return "", missing
		}

		f.chains[chain] = []string{}
	case "-X":
		if !exists || len(rules) > 0 {
			return "", missing
		}

		for name, rs := range f.chains {
			if !strings.HasPrefix(name, table) || (table == "" && strings.Contains(name, "/")) {
				continue // another table's chains cannot refer to this one
			}

			for _, r := range rs {
				if strings.HasSuffix(r, "-j "+args[1]) {
					return "", errors.New("iptables: Too many links")
				}
			}
		}

		delete(f.chains, chain)
	case "-A":
		f.chains[chain] = append(rules, rule)
	case "-I": // -I INPUT 1 <rule>
		f.chains[chain] = append([]string{strings.Join(args[3:], " ")}, rules...)
	case "-C":
		if !slices.Contains(rules, rule) {
			return "", missing
		}
	case "-D":
		i := slices.Index(rules, rule)
		if i < 0 {
			return "", missing
		}

		f.chains[chain] = slices.Delete(rules, i, i+1)
	}

	return "", nil
}

func (f *fakeTables) guard() *Guard {
	return &Guard{Run: f.run, Port: 20999, Sysctl: func(string, string) error { return nil }}
}

var guarded = []string{
	"-m conntrack --ctstate ESTABLISHED,RELATED -j RETURN",
	"-p tcp -d 10.231.5.1 --dport 20999 -j ACCEPT",
	"-j DROP",
}

func TestTheGuardInstallsOneChainAndOneJumpAndIsIdempotent(t *testing.T) {
	f := newFakeTables()
	g := f.guard()
	a := Addr{Slot: 5}

	for range 2 {
		if err := g.Install(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}

	if got := f.chains["SBX-FC5"]; !slices.Equal(got, guarded) {
		t.Fatalf("chain = %q, want %q", got, guarded)
	}

	// First in INPUT, exactly once, matched to this bridge only; everything else untouched.
	want := []string{"-i sbxfc5 -j SBX-FC5", "-i eth0 -j ACCEPT", "-i docker0 -j DOCKER-IN"}
	if got := f.chains["INPUT"]; !slices.Equal(got, want) {
		t.Fatalf("INPUT = %q, want %q", got, want)
	}

	// Filled before the jump exists: the -I is the last command of a fresh install.
	fresh := newFakeTables()
	_ = fresh.guard().Install(context.Background(), a)

	if last := fresh.cmds[len(fresh.cmds)-1]; !strings.HasPrefix(last, "-I INPUT 1 -i sbxfc5") {
		t.Fatalf("the jump was not the last thing made: %q", fresh.cmds)
	}
}

func TestTheGuardReleasesExactlyWhatItMadeAndIsIdempotent(t *testing.T) {
	f := newFakeTables()
	g := f.guard()
	ctx := context.Background()

	for _, s := range []int{5, 9} {
		if err := g.Install(ctx, Addr{Slot: s}); err != nil {
			t.Fatal(err)
		}
	}

	// A second jump, as two racing processes could leave.
	f.chains["INPUT"] = append([]string{"-i sbxfc5 -j SBX-FC5"}, f.chains["INPUT"]...)

	for range 2 {
		if err := g.Release(ctx, Addr{Slot: 5}); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok := f.chains["SBX-FC5"]; ok {
		t.Fatal("the chain outlived its bridge")
	}

	want := []string{"-i sbxfc9 -j SBX-FC9", "-i eth0 -j ACCEPT", "-i docker0 -j DOCKER-IN"}
	if got := f.chains["INPUT"]; !slices.Equal(got, want) {
		t.Fatalf("INPUT after release = %q, want %q (slot 9's and others' kept)", got, want)
	}

	if !slices.Equal(f.chains["SBX-FC9"], []string{guarded[0], strings.ReplaceAll(guarded[1], ".5.", ".9."), guarded[2]}) ||
		!slices.Equal(f.chains["OTHERS"], []string{"-j RETURN"}) {
		t.Fatalf("release touched another chain: %v", f.chains)
	}
}

func TestAGuardThatFailsHalfwayLeavesNothingBehind(t *testing.T) {
	for _, step := range []string{"-A SBX-FC5 -m conntrack", "-A SBX-FC5 -j DROP", "-I INPUT"} {
		f := newFakeTables()
		f.fail = step

		if err := f.guard().Install(context.Background(), Addr{Slot: 5}); err == nil {
			t.Fatalf("%s failing was not reported", step)
		}

		if _, ok := f.chains["SBX-FC5"]; ok || len(f.chains["INPUT"]) != 2 {
			t.Fatalf("%s failing left %v", step, f.chains)
		}
	}
}

func TestNoIptablesIsReportedAndReleaseIsANoOp(t *testing.T) {
	f := newFakeTables()
	f.absent = true
	g := f.guard()

	if err := g.Install(context.Background(), Addr{Slot: 5}); !errors.Is(err, ErrNoFirewall) {
		t.Fatalf("Install without iptables = %v", err)
	}

	if err := g.Release(context.Background(), Addr{Slot: 5}); err != nil {
		t.Fatalf("Release without iptables = %v", err)
	}
}

func TestNoIPv6TurnsItOffOnTheBridgeOnly(t *testing.T) {
	var wrote []string

	g := &Guard{Sysctl: func(p, v string) error {
		wrote = append(wrote, p+"="+v)
		return nil
	}}

	if err := g.NoIPv6(Addr{Slot: 5}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(wrote, []string{"/proc/sys/net/ipv6/conf/sbxfc5/disable_ipv6=1"}) {
		t.Fatalf("wrote %q", wrote)
	}

	g.Sysctl = func(string, string) error { return os.ErrNotExist } // a kernel without IPv6
	if err := g.NoIPv6(Addr{Slot: 5}); err != nil {
		t.Fatalf("no IPv6 at all = %v", err)
	}
}

// The bridge is guarded before it is up, and the guard goes with it.
func TestABridgeIsGuardedBeforeItIsUpAndReleasedWithIt(t *testing.T) {
	ip := &fakeIP{links: map[string]bool{}}
	tables := newFakeTables()

	var order []string

	g := tables.guard()
	run := g.Run
	g.Run = func(ctx context.Context, args ...string) (string, error) {
		order = append(order, "iptables "+strings.Join(args, " "))
		return run(ctx, args...)
	}
	g.Sysctl = func(p, _ string) error {
		order = append(order, "sysctl "+p)
		return nil
	}

	n := &IPNetwork{Owner: -1, Guard: g, Run: func(ctx context.Context, args ...string) (string, error) {
		order = append(order, "ip "+strings.Join(args, " "))
		return ip.run(ctx, args...)
	}}

	a := Addr{Slot: 5, Index: 0}
	if err := n.EnsureTap(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	jump := slices.Index(order, "iptables -I INPUT 1 -i sbxfc5 -j SBX-FC5")
	v6 := slices.Index(order, "sysctl /proc/sys/net/ipv6/conf/sbxfc5/disable_ipv6")
	up := slices.Index(order, "ip link set sbxfc5 up")

	if jump < 0 || v6 < 0 || up < 0 || jump > up || v6 > up {
		t.Fatalf("guard (%d), ipv6 off (%d) and bridge up (%d) out of order:\n%s", jump, v6, up, strings.Join(order, "\n"))
	}

	// A second VM on the bridge costs checks, never a write: six `-C`s while the guard is whole.
	before := len(tables.cmds)
	if err := n.EnsureTap(context.Background(), Addr{Slot: 5, Index: 1}); err != nil {
		t.Fatal(err)
	}

	if w := writes(tables.cmds[before:]); len(w) > 0 || len(tables.cmds)-before > 6 {
		t.Fatalf("EnsureTap on an existing bridge ran %q", tables.cmds[before:])
	}

	if err := n.RemoveBridge(context.Background(), 5); err != nil {
		t.Fatal(err)
	}

	if _, ok := tables.chains["SBX-FC5"]; ok || slices.Contains(tables.chains["INPUT"], "-i sbxfc5 -j SBX-FC5") {
		t.Fatalf("rules outlived the bridge: %v", tables.chains)
	}
}
