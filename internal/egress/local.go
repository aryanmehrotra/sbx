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
