package fc

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// A docker-published port is not an INPUT packet. Docker's nat PREROUTING DNATs anything
// addressed to a local address (addrtype LOCAL) onto the container, so a guest dialling
// 10.231.<slot>.1:<published port> is FORWARD traffic by the time the filter table sees it -
// and DOCKER's chain accepts it. The same goes for a guest dialling a container's IP, or a
// kube-proxy NodePort. So the guard also drops, in mangle (which docker does not write and
// which runs before nat), everything a guest starts except the filter port, and everything
// forwarded from or to the bridge.
var guardedMangle = []string{
	"-m conntrack --ctstate ESTABLISHED,RELATED -j RETURN",
	"-p tcp -d 10.231.5.1 --dport 20999 -j RETURN",
	"-j DROP",
}

func TestTheGuardDropsWhatDockerWouldDNATOrForward(t *testing.T) {
	f := newFakeTables()
	g := f.guard()
	a := Addr{Slot: 5}

	fresh := 0

	for range 2 {
		if err := g.Install(context.Background(), a); err != nil {
			t.Fatal(err)
		}

		if fresh == 0 {
			fresh = len(f.cmds)
		}
	}

	if got := f.chains["mangle/SBX-FC5"]; !slices.Equal(got, guardedMangle) {
		t.Fatalf("mangle chain = %q, want %q", got, guardedMangle)
	}

	// First in PREROUTING, before nat's DNAT can see the packet; somebody else's rules kept.
	if got, want := f.chains["mangle/PREROUTING"], []string{"-i sbxfc5 -j SBX-FC5", "-j CNI-MARK"}; !slices.Equal(got, want) {
		t.Fatalf("mangle PREROUTING = %q, want %q", got, want)
	}

	// Nothing is forwarded from the bridge or onto it: no route off the host is ever meant.
	fwd := f.chains["mangle/FORWARD"]
	for _, r := range []string{"-i sbxfc5 -j DROP", "-o sbxfc5 -j DROP"} {
		if n := count(fwd, r); n != 1 {
			t.Fatalf("mangle FORWARD has %q %d times: %q", r, n, fwd)
		}
	}

	// The filter table is untouched by all this: docker's FORWARD chain is docker's.
	if _, ok := f.chains["FORWARD"]; ok {
		t.Fatalf("the guard wrote to the filter table's FORWARD: %v", f.chains["FORWARD"])
	}

	// Every chain filled before any rule reaches one.
	lastFill, firstHook := -1, fresh

	for i, c := range f.cmds[:fresh] {
		if strings.Contains(c, "-A SBX-FC5") {
			lastFill = i
		}

		if strings.Contains(c, " -I ") && firstHook == fresh {
			firstHook = i
		}
	}

	if lastFill > firstHook {
		t.Fatalf("a hook was inserted before every chain was filled: %q", f.cmds)
	}

	if err := g.Release(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	if _, ok := f.chains["mangle/SBX-FC5"]; ok ||
		!slices.Equal(f.chains["mangle/PREROUTING"], []string{"-j CNI-MARK"}) || len(f.chains["mangle/FORWARD"]) != 0 {
		t.Fatalf("release left mangle rules: %v", f.chains)
	}
}

// A failure at any step of the mangle half leaves no rule of sbx's in any table.
func TestAGuardThatFailsInMangleLeavesNothingBehind(t *testing.T) {
	for _, step := range []string{
		"-t mangle -N SBX-FC5", "-t mangle -A SBX-FC5 -j DROP", "-t mangle -I PREROUTING",
		"-t mangle -I FORWARD 1 -o", "-t mangle -I FORWARD 1 -i",
	} {
		f := newFakeTables()
		f.fail = step

		if err := f.guard().Install(context.Background(), Addr{Slot: 5}); err == nil {
			t.Fatalf("%s failing was not reported", step)
		}

		for name, rules := range f.chains {
			if strings.Contains(name, "SBX-FC") {
				t.Fatalf("%s failing left chain %s", step, name)
			}

			for _, r := range rules {
				if strings.Contains(r, "sbxfc5") {
					t.Fatalf("%s failing left %s: %q", step, name, r)
				}
			}
		}
	}
}

func count(rules []string, r string) int {
	n := 0

	for _, x := range rules {
		if x == r {
			n++
		}
	}

	return n
}
