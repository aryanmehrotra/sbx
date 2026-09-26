package egress

import (
	"net/netip"
	"testing"
)

// Under egress: "allow" a microVM's filter carried a guest to docker container IPs, the host's
// LAN and the cloud VPC - all private, none reachable from the no-NAT bridge on its own. A VM
// filter now refuses every private range and every neighbour on a host interface's prefix, and
// only the operator's widen list (sbx serve) lifts that - never the sandbox's own policy.
func TestAVMFilterRefusesPrivateRangesAndHostSubnetsUnlessTheOperatorWidens(t *testing.T) {
	addrs := fakeAddrs("192.168.5.15", "172.17.0.1", "203.0.113.7") // fakeAddrs gives each a /24

	refuse := vmRefuse(addrs, vmPlan, nil)
	for s, want := range map[string]bool{
		"10.0.0.5":        true, // a VPC neighbour
		"172.17.0.3":      true, // a docker container
		"172.20.1.1":      true,
		"192.168.1.1":     true, // a LAN router on another subnet
		"100.64.0.1":      true, // CGNAT / tailscale
		"fd00::1":         true, // ULA
		"169.254.169.254": true, // metadata
		"203.0.113.9":     true, // a public neighbour on the host's own prefix (Contains, not ==)
		"0.1.2.3":         true, // "this network"
		"10.231.5.1":      true,
		"127.0.0.1":       true,
		"93.184.215.14":   false,
		"2606:4700::1111": false,
	} {
		if got := refuse(netip.MustParseAddr(s)); got != want {
			t.Errorf("%s: refused=%v, want %v", s, got, want)
		}
	}

	// The operator may open a private range - but never the host itself, its loopback or the plan.
	widened := vmRefuse(addrs, vmPlan, []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("0.0.0.0/0")})
	for s, want := range map[string]bool{
		"172.17.0.3": false,
		"10.0.0.5":   false,
		"172.17.0.1": true, // the host's own docker0 address
		"10.231.7.2": true,
		"127.0.0.1":  true,
	} {
		if got := widened(netip.MustParseAddr(s)); got != want {
			t.Errorf("widened %s: refused=%v, want %v", s, got, want)
		}
	}
}
