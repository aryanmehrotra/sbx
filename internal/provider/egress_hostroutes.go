package provider

// Whether this host already routes part of the block sbx addresses its no-NAT bridges out of.
//
// 198.18.0.0/15 was picked because it is reserved and nothing hands addresses out of it
// (egressSubnetBlock). Reserved is not unused: it is a common home for a local proxy's fake-IP
// range, and a tool like that installs a host route over the whole block on a tunnel interface.
// A bridge addressed inside that range then collides with it - two ways to the same addresses,
// resolved by prefix length - and whichever traffic takes the losing one goes nowhere, with no
// error anywhere to say so.
//
// So the routes are read once per create, a slice the host already routes is skipped, and the
// refusal names the route when no slice is free. An unreadable table is not a refusal: a create
// that stopped because a routing table did not parse would be a worse outcome than the collision
// it guards against, and the collision is what the platform did before this existed.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// hostRoute is one of the host's routes, as much of it as this needs: where it goes and what the
// operator would recognise it by.
type hostRoute struct {
	prefix netip.Prefix
	iface  string
}

func (r hostRoute) String() string {
	if r.iface == "" {
		return r.prefix.String()
	}

	return r.prefix.String() + " on " + r.iface
}

// withinEgressBlock reports whether a route is one a bridge could collide with: inside sbx's block,
// and at least as specific as that block itself.
//
// A broader route is not a collision, which the default route makes worth stating: every machine
// has one, and 198.18.0.0/15 is reserved, so addresses in it were going nowhere useful down that
// route and a /24 bridge taking them is the point. What collides is a route more specific than the
// whole block - a /16 over half of it loses its own addresses to the bridge, and a /25 or a host
// route inside a bridge's /24 wins over the bridge outright.
func withinEgressBlock(p netip.Prefix) bool {
	block := netip.MustParsePrefix(egressSubnetBlock)

	return p.Bits() >= block.Bits() && block.Contains(p.Addr())
}

// routedElsewhere names the host route a bridge on this subnet would collide with, or "".
func routedElsewhere(routes []hostRoute, subnet netip.Prefix) string {
	for _, r := range routes {
		if r.prefix.Overlaps(subnet) {
			return r.String()
		}
	}

	return ""
}

// hostRoutes is the host's IPv4 routes that touch the egress block, and nothing else.
//
// Narrow on purpose. Reading the whole table portably without a dependency is a parser for every
// platform's output format, and the root go.mod has no require (AGENTS.md, rule 1); the question
// here is only ever about one /15, so everything outside it is dropped unparsed.
//
// Docker's own bridges are dropped too. On native Linux a sandbox bridge IS a host route once it
// exists, so a second sandbox hashing to a taken slice would otherwise be refused with a message
// about a foreign route. Docker's overlap check is what answers that case, and it already does.
func hostRoutes() []hostRoute {
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return interfaceRoutes()
		}

		return append(procNetRoutes(b), interfaceRoutes()...)
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		ctx, cancel := commandTimeout()
		defer cancel()

		out, err := exec.CommandContext(ctx, "netstat", "-rn", "-f", "inet").Output()
		if err != nil {
			return interfaceRoutes()
		}

		return append(netstatRoutes(out), interfaceRoutes()...)
	default:
		// Windows and anything else: the addresses the host holds are still readable, and a
		// tunnel interface in the block holds one.
		return interfaceRoutes()
	}
}

// commandTimeout bounds the one command this reads the table with. A routing table that takes
// longer than this is a machine with other problems, and a create must not hang on it.
func commandTimeout() (ctx context.Context, cancel context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// interfaceRoutes is every address this machine holds inside the egress block, as the subnet it
// sits in. A tunnel interface carrying a fake-IP range holds one, which is the case this is for,
// and it is the one reading that works on every platform.
func interfaceRoutes() []hostRoute {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var out []hostRoute

	for _, ifi := range ifaces {
		if skipIface(ifi.Name) {
			continue
		}

		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}

		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}

			p, ok := prefixOf(n)
			if !ok || !withinEgressBlock(p) {
				continue
			}

			out = append(out, hostRoute{prefix: p, iface: ifi.Name})
		}
	}

	return out
}

// prefixOf is a net.IPNet as the IPv4 prefix it covers.
func prefixOf(n *net.IPNet) (netip.Prefix, bool) {
	v4 := n.IP.To4()
	if v4 == nil {
		return netip.Prefix{}, false
	}

	ones, bits := n.Mask.Size()
	if bits != 32 {
		return netip.Prefix{}, false
	}

	addr, ok := netip.AddrFromSlice(v4)
	if !ok {
		return netip.Prefix{}, false
	}

	return netip.PrefixFrom(addr, ones).Masked(), true
}

// procNetRoutes reads Linux's table. Every field is little-endian hex of a network-order value,
// so the destination and mask are read as uint32 and turned back into an address.
func procNetRoutes(table []byte) []hostRoute {
	var out []hostRoute

	sc := bufio.NewScanner(bytes.NewReader(table))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[0] == "Iface" {
			continue
		}

		dst, ok := leHex(f[1])
		if !ok {
			continue
		}

		mask, ok := leHex(f[7])
		if !ok {
			continue
		}

		bits := maskBits(mask)
		if bits < 0 || skipIface(f[0]) {
			continue
		}

		var b [4]byte
		binary.BigEndian.PutUint32(b[:], dst)

		p := netip.PrefixFrom(netip.AddrFrom4(b), bits).Masked()
		if !withinEgressBlock(p) {
			continue
		}

		out = append(out, hostRoute{prefix: p, iface: f[0]})
	}

	return out
}

// leHex is one /proc/net/route field: eight hex digits of a network-order value, written
// little-endian.
func leHex(s string) (uint32, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 4 {
		return 0, false
	}

	return binary.LittleEndian.Uint32(b), true
}

// maskBits is a contiguous netmask as a prefix length, or -1 when it is not contiguous.
func maskBits(mask uint32) int {
	bits := 0
	for bits < 32 && mask&(1<<(31-uint(bits))) != 0 {
		bits++
	}

	if mask<<uint(bits) != 0 {
		return -1
	}

	return bits
}

// netstatRoutes reads the BSD table `netstat -rn -f inet` prints: a destination column that may be
// a bare address, a prefix, or either with trailing octets left off, and a Netif column.
func netstatRoutes(table []byte) []hostRoute {
	out := []hostRoute{}
	netif := -1

	sc := bufio.NewScanner(bytes.NewReader(table))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}

		if strings.EqualFold(f[0], "Destination") { //nolint:nestif // one header line, one shape
			netif = -1

			for i, h := range f {
				if strings.EqualFold(h, "Netif") {
					netif = i
				}
			}

			continue
		}

		p, ok := netstatPrefix(f[0])
		if !ok || !withinEgressBlock(p) {
			continue
		}

		name := ""
		if netif >= 0 && netif < len(f) {
			name = f[netif]
		}

		if skipIface(name) {
			continue
		}

		out = append(out, hostRoute{prefix: p, iface: name})
	}

	return out
}

// netstatPrefix turns a BSD destination column into a prefix. "198.18/16" and "198.18.0.0/16" are
// the same route; a bare "198.18.9.7" is a host route, and "198.18" with no length is as many bits
// as it has octets, which is how netstat abbreviates one.
func netstatPrefix(dst string) (netip.Prefix, bool) {
	addr, bits, slash := strings.Cut(dst, "/")

	octets := strings.Split(addr, ".")
	if len(octets) == 0 || len(octets) > 4 {
		return netip.Prefix{}, false
	}

	given := len(octets)
	for len(octets) < 4 {
		octets = append(octets, "0")
	}

	ip, err := netip.ParseAddr(strings.Join(octets, "."))
	if err != nil || !ip.Is4() {
		return netip.Prefix{}, false
	}

	n := 8 * given
	if slash {
		if n, err = strconv.Atoi(bits); err != nil || n < 0 || n > 32 {
			return netip.Prefix{}, false
		}
	}

	return netip.PrefixFrom(ip, n).Masked(), true
}

// skipIface drops the interfaces whose routes are not a conflict: docker's own bridges, which on
// native Linux include every sandbox bridge sbx has already made, and loopback.
func skipIface(name string) bool {
	switch {
	case name == "":
		return false
	case strings.HasPrefix(name, "br-"), strings.HasPrefix(name, "docker"):
		return true
	case strings.HasPrefix(name, "lo"):
		return true
	}

	return false
}

// routeConflict is the refusal when every slice this sandbox would use is already routed.
func routeConflict(sandbox string, collided []string) error {
	return fmt.Errorf("the egress bridge for %s cannot be addressed: every /24 of %s it would "+
		"take is already routed on this host (%s). A bridge there would collide with that route, "+
		"the host would send each address to whichever prefix is longer, and the traffic that went "+
		"the other way would fail with nothing to say why - so it is refused rather than created. "+
		"Check it with `netstat -rn | grep 198.18` (`ip route | grep 198.18` on Linux), and remove "+
		"the route or stop the tool holding it - a local proxy's fake-IP range lives in this "+
		"block - then create the sandbox again", sandbox, egressSubnetBlock,
		strings.Join(collided, "; "))
}
