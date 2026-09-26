package egress

import (
	"net"
	"net/netip"
)

// OnThisHost is a Filter.Refuse for a filter that runs on the host its sandbox is kept off: it
// reports any address in ranges, any address assigned to one of this host's interfaces, and
// everything HostLocal does (loopback, link-local, unspecified, multicast).
//
// The interfaces are read per call rather than once, because the set changes under a running
// filter - every microVM sandbox created after it adds a bridge address - and one read is a
// netlink dump costing microseconds against a connection that is about to cross the internet.
// If they cannot be read the answer is "on this host": a filter that cannot tell must not be
// the one that opens the door.
func OnThisHost(ranges ...netip.Prefix) func(netip.Addr) bool {
	return onThisHost(net.InterfaceAddrs, ranges)
}

func onThisHost(addrs func() ([]net.Addr, error), ranges []netip.Prefix) func(netip.Addr) bool {
	return func(a netip.Addr) bool {
		a = a.Unmap()

		if hostLocal(a) {
			return true
		}

		for _, p := range ranges {
			if p.Contains(a) {
				return true
			}
		}

		list, err := addrs()
		if err != nil {
			return true
		}

		for _, x := range list {
			n, ok := x.(*net.IPNet)
			if !ok {
				continue
			}

			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == a {
				return true
			}
		}

		return false
	}
}

// Private is what a microVM's filter refuses by default beyond the host itself: RFC 1918, CGNAT
// (100.64.0.0/10, also tailscale's), IPv6 unique-local, and "this network" (0.0.0.0/8, which
// Linux dials as the local host). Link-local is refused already, as HostLocal.
var Private = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// VMRefuse is the Filter.Refuse for a microVM's filter. It refuses everything OnThisHost(plan)
// does - never widened: the host, its loopback and every guest - and, unless widen names it,
// every Private address and every address on the prefix of one of the host's interfaces (its
// LAN, its VPC subnet, a docker bridge's containers), none of which a guest on its no-NAT bridge
// could reach on its own.
//
// widen is the operator's (sbx serve --vm-egress-allow), not the sandbox's: a sandbox's policy
// is written by its API caller, and cannot open any of this.
func VMRefuse(plan netip.Prefix, widen []netip.Prefix) func(netip.Addr) bool {
	return vmRefuse(net.InterfaceAddrs, plan, widen)
}

func vmRefuse(addrs func() ([]net.Addr, error), plan netip.Prefix, widen []netip.Prefix) func(netip.Addr) bool {
	host := onThisHost(addrs, []netip.Prefix{plan})

	return func(a netip.Addr) bool {
		a = a.Unmap()

		switch {
		case host(a):
			return true
		case contains(widen, a):
			return false
		case contains(Private, a):
			return true
		}

		list, err := addrs()
		if err != nil {
			return true
		}

		for _, x := range list {
			n, ok := x.(*net.IPNet)
			if !ok {
				continue
			}

			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}

			ones, bits := n.Mask.Size()
			if ip.Is4In6() {
				ip = ip.Unmap()
				if bits == 128 {
					ones -= 96
				}
			}

			if bits == 0 || ones < 0 {
				continue // a non-canonical mask: no prefix to speak of
			}

			if p, err := ip.Prefix(ones); err == nil && p.Contains(a) {
				return true
			}
		}

		return false
	}
}
