package fc

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// K6(b): a jailed VMM runs in a network namespace of its own. Its tap is made THERE, owned by the
// VM's uid, bridged inside the namespace to one end of a veth pair whose other end is a port of the
// sandbox's bridge on the host - so the guest's frames reach the host exactly where a host tap's
// did (the guard's rules match on the bridge), while the VMM process itself has no interface, no
// address and no route on the host's network: an escape reaches what the guest reaches.
func TestAPerVMNetnsPutsTheTapInsideAndAVethOnTheBridge(t *testing.T) {
	f := &fakeIP{links: map[string]bool{}}
	n := &IPNetwork{Run: f.run, Owner: -1, PerVMNetNS: true, OwnerOf: func(Addr) int { return 900517 }}
	a := Addr{Slot: 2, Index: 5}

	if err := n.EnsureTap(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	ns := a.NetNS()
	if ns != "sbxfc2-5" || NetNSPath(a) != "/var/run/netns/sbxfc2-5" {
		t.Fatalf("netns %q at %q", ns, NetNSPath(a))
	}

	want := []string{
		"netns add sbxfc2-5",
		"link add sbxfc2-5 type veth peer name eth0 netns sbxfc2-5",
		"link set sbxfc2-5 master sbxfc2",
		"link set sbxfc2-5 up",
		"-n sbxfc2-5 link add br0 type bridge",
		"-n sbxfc2-5 link set eth0 master br0",
		"-n sbxfc2-5 tuntap add dev sbxfc2-5 mode tap user 900517",
		"-n sbxfc2-5 link set sbxfc2-5 master br0",
	}

	last := -1

	for _, w := range want {
		i := slices.Index(f.cmds, w)
		if i < 0 || i < last {
			t.Fatalf("missing or out of order %q in\n%s", w, strings.Join(f.cmds, "\n"))
		}

		last = i
	}

	// No link-local address inside: an interface the VMM could bind to is one it could use.
	for _, dev := range []string{"br0", "eth0", "sbxfc2-5"} {
		if !slices.Contains(f.cmds, "-n sbxfc2-5 link set dev "+dev+" addrgenmode none") ||
			!slices.Contains(f.cmds, "-n sbxfc2-5 link set "+dev+" up") {
			t.Fatalf("%s in the netns is not up without IPv6 addresses:\n%s", dev, strings.Join(f.cmds, "\n"))
		}
	}

	for _, c := range f.cmds {
		if strings.HasPrefix(c, "tuntap add") {
			t.Fatalf("a tap was made on the host's network: %q", c)
		}
	}

	// Rebuilt from nothing at each launch: a namespace or a host tap left by an earlier VMM (a v0.12
	// tap on the host, a namespace a reboot kept the name of) is never reused.
	f.cmds = nil

	if err := n.EnsureTap(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	if i, j := slices.Index(f.cmds, "netns del sbxfc2-5"), slices.Index(f.cmds, "netns add sbxfc2-5"); i < 0 || j < i {
		t.Fatalf("a second launch did not replace the namespace:\n%s", strings.Join(f.cmds, "\n"))
	}

	if i, j := slices.Index(f.cmds, "link del sbxfc2-5"), slices.Index(f.cmds, "netns add sbxfc2-5"); i < 0 || j < i {
		t.Fatalf("a second launch kept the host end of the old pair:\n%s", strings.Join(f.cmds, "\n"))
	}

	f.cmds = nil

	if err := n.RemoveTap(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(f.cmds, "netns del sbxfc2-5") {
		t.Fatalf("RemoveTap left the namespace: %v", f.cmds)
	}
}

// The jailer joins the namespace before it drops to the VM's uid (v1.17.0 env.rs: join_netns in
// run(), before the chroot): --netns <path>, among the jailer's own arguments.
func TestJailerArgvJoinsTheVMsNetns(t *testing.T) {
	s := LaunchSpec{Binary: "/cache/firecracker", Dir: "/state/vms/0123456789abcdef", Jail: &JailSpec{
		Jailer: "/cache/jailer", UID: 900517, GID: 900517, NetNS: "/var/run/netns/sbxfc2-5",
	}}

	got := JailerArgs(s)
	at, end := slices.Index(got, "--netns"), slices.Index(got, "--")

	if at < 0 || at > end || got[at+1] != "/var/run/netns/sbxfc2-5" {
		t.Fatalf("argv %q: want --netns /var/run/netns/sbxfc2-5 among the jailer's own arguments", got)
	}

	s.Jail.NetNS = ""
	if slices.Contains(JailerArgs(s), "--netns") {
		t.Fatalf("no netns still passed one: %q", JailerArgs(s))
	}
}
