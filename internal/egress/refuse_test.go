package egress

import (
	"context"
	"net/netip"
	"testing"
)

// A filter hosted on the machine its sandbox is kept off must not be talked into dialling that
// machine by the sandbox's own policy. A caller-written allow rule for 0.0.0.0/0, 10.0.0.0/8 or
// 127.0.0.1 used to override the refusal, so the root-run filter would CONNECT to the host's
// loopback (the OpenSandbox API, the daemon's ports), the host's addresses and other guests.
func TestAPolicyAllowRuleDoesNotOpenWhatTheHostRefuses(t *testing.T) {
	f := NewPolicy(Policy{DefaultAction: ActionAllow, Egress: []Rule{}})
	f.Refuse = onThisHost(fakeAddrs("192.168.5.15"), []netip.Prefix{vmPlan})
	f.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}

	for _, target := range []string{"0.0.0.0/0", "10.0.0.0/8", "127.0.0.1", "169.254.169.254", "::/0", "192.168.5.15"} {
		p, err := Policy{DefaultAction: ActionDeny, Egress: []Rule{
			{Action: ActionAllow, Target: target},
			{Action: ActionAllow, Target: "loop.example"},
		}}.Normalize()
		if err != nil {
			t.Fatal(err)
		}

		if err := f.SetPolicy(p); err != nil {
			t.Fatal(err)
		}

		for _, host := range []string{"127.0.0.1", "127.9.9.9", "::1", "10.231.5.1", "10.231.7.2", "192.168.5.15",
			"169.254.169.254", "0.0.0.0", "::ffff:127.0.0.1"} {
			if f.Permits(host) {
				t.Errorf("allow %s opened %s", target, host)
			}
		}

		if _, err := f.admit(context.Background(), "loop.example"); err == nil {
			t.Errorf("allow %s let a name resolving to loopback through", target)
		}
	}
}

// HostLocal is what a docker filter the daemon hosts on the host refuses whatever its policy
// says: that filter's loopback is the host's.
func TestHostLocalIsLoopbackLinkLocalAndUnspecified(t *testing.T) {
	for s, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "169.254.169.254": true, "fe80::1": true, "0.0.0.0": true,
		"224.0.0.1": true, "93.184.215.14": false, "10.1.2.3": false,
	} {
		if got := HostLocal(netip.MustParseAddr(s)); got != want {
			t.Errorf("HostLocal(%s) = %v, want %v", s, got, want)
		}
	}
}
