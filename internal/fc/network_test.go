package fc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestAddrIsArithmetic(t *testing.T) {
	a := Addr{Slot: 7, Index: 3}

	for got, want := range map[string]string{
		a.Bridge(): "sbxfc7", a.Tap(): "sbxfc7-3", a.Gateway(): "10.231.7.1", a.GuestIP(): "10.231.7.5",
		a.MAC(): "06:00:0a:e7:07:05", a.BootArg(): "ip=10.231.7.5::10.231.7.1:255.255.255.0::eth0:off",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}

	// The largest slot and index the port plan produces still fit a Linux interface name.
	if n := len(Addr{Slot: 59, Index: 19}.Tap()); n > 15 {
		t.Errorf("tap name is %d bytes, over IFNAMSIZ", n)
	}

	if (Addr{Slot: 300}).Valid() == nil || (Addr{Index: -1}).Valid() == nil {
		t.Error("out-of-plan addresses accepted")
	}
}

type fakeIP struct {
	links map[string]bool
	cmds  []string
	fail  string
}

func (f *fakeIP) run(_ context.Context, args ...string) (string, error) {
	cmd := strings.Join(args, " ")
	f.cmds = append(f.cmds, cmd)

	if f.fail != "" && strings.HasPrefix(cmd, f.fail) {
		return "", errors.New("RTNETLINK answers: Operation not permitted")
	}

	switch {
	case args[0] == "link" && args[1] == "show":
		if !f.links[args[3]] {
			return "", errors.New("does not exist")
		}
	case args[0] == "link" && args[1] == "add":
		f.links[args[2]] = true
	case args[0] == "tuntap":
		f.links[args[3]] = true
	case args[0] == "link" && args[1] == "del":
		delete(f.links, args[2])
	}

	return "", nil
}

func TestEnsureTapIsIdempotent(t *testing.T) {
	f := &fakeIP{links: map[string]bool{}}
	n := &IPNetwork{Run: f.run, Owner: 1000}
	a := Addr{Slot: 2, Index: 0}

	if err := n.EnsureTap(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"link add sbxfc2 type bridge", "addr add 10.231.2.1/24 dev sbxfc2", "link set sbxfc2 up",
		"tuntap add dev sbxfc2-0 mode tap user 1000", "link set sbxfc2-0 master sbxfc2", "link set sbxfc2-0 up",
	} {
		if !slices.Contains(f.cmds, want) {
			t.Fatalf("missing %q in %v", want, f.cmds)
		}
	}

	f.cmds = nil

	if err := n.EnsureTap(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	for _, c := range f.cmds {
		if strings.Contains(c, " add ") {
			t.Fatalf("second EnsureTap re-created something: %v", f.cmds)
		}
	}

	if err := n.RemoveTap(context.Background(), a); err != nil || f.links["sbxfc2-0"] {
		t.Fatalf("RemoveTap: %v, links %v", err, f.links)
	}

	if err := n.RemoveTap(context.Background(), a); err != nil {
		t.Fatalf("removing an absent tap: %v", err)
	}

	if err := n.RemoveBridge(context.Background(), 2); err != nil || f.links["sbxfc2"] {
		t.Fatalf("RemoveBridge: %v", err)
	}
}

func TestEnsureTapSaysWhatFailed(t *testing.T) {
	f := &fakeIP{links: map[string]bool{}, fail: "tuntap"}
	n := &IPNetwork{Run: f.run, Owner: -1}

	err := n.EnsureTap(context.Background(), Addr{Slot: 1})
	if err == nil || !strings.Contains(err.Error(), "creating tap sbxfc1-0") || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("err = %v", err)
	}

	for _, c := range f.cmds {
		if strings.Contains(c, "user") {
			t.Fatal("Owner -1 still passed a user")
		}
	}
}
