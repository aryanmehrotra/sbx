package egress

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Doors is the Filter.Refuse of a filter that runs as a container: the addresses behind which sit
// the machine docker runs on, and the machine behind that.
//
// On colima or Docker Desktop the container filter is dual-homed inside a Linux VM. Everything in
// that VM is one hop from it - its own gateway on each network is the VM, and so is every other
// docker network's gateway - and the VM forwards host.lima.internal and host.docker.internal to
// the Mac's loopback. Measured before this existed, under `egress: "allow"`, from inside a
// sandbox: `CONNECT host.lima.internal:<port>` reached a listener bound only to the Mac's
// 127.0.0.1, and `CONNECT 172.17.0.1:22` answered with the VM's own sshd. The workload has no
// route to any of those; the filter handed it one. (This file used to say the container filter has
// only its own loopback behind it. On a VM-backed docker that is false.)
//
// Like Refuse on the other filters, no allow rule opens these. A sandbox's policy is written by
// the sandbox's caller, and must not be able to write itself a door onto the machine that runs it.
//
// What is refused:
//   - Static: given by the provider at start - the gateway of every docker network on the engine,
//     and the default bridge's whole subnet (the filter is on it for a route out, not to reach the
//     other containers there, which the sandbox cannot reach on its own).
//   - every gateway in this container's routing table, read again on each Refresh;
//   - the /24 (or /64) around what HostDoorNames resolve to here. The whole prefix, not the one
//     address, because it is the VM's link to the host: on colima 192.168.5.2 is the Mac and
//     192.168.5.15 the VM itself, and on Docker Desktop 192.168.65.0/24 is the same arrangement.
//
// What it cannot see: a docker network created after the filter started has a gateway on the VM
// that is in neither list until the filter is recreated. With ports limited to 80 and 443 (see
// DefaultPorts) that door is only to what the VM itself serves on those ports.
type Doors struct {
	// Static is what the provider named at start.
	Static []netip.Prefix

	// Resolve looks a name up, or is nil for the system resolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	// Routes returns the contents of /proc/net/route, or is nil to read it.
	Routes func() (string, error)

	set atomic.Pointer[[]netip.Prefix]
}

// HostDoorNames are the names a docker engine gives the machine it runs on, or the one behind it.
var HostDoorNames = []string{"host.docker.internal", "host.lima.internal", "gateway.docker.internal"}

// ParsePrefixes reads a comma-separated list of addresses and CIDRs; an address is its own /32 or
// /128.
func ParsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix

	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}

		if a, err := netip.ParseAddr(f); err == nil {
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))

			continue
		}

		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("%q is neither an address nor a CIDR", f)
		}

		out = append(out, unmapPrefix(p.Masked()))
	}

	return out, nil
}

// Refresh recomputes the refused set. It never fails: a name that does not resolve is a door that
// is not there, and a routing table that cannot be read leaves the static list and the names.
func (d *Doors) Refresh(ctx context.Context) {
	set := append([]netip.Prefix(nil), d.Static...)

	routes := d.Routes
	if routes == nil {
		routes = func() (string, error) {
			b, err := os.ReadFile("/proc/net/route")
			return string(b), err
		}
	}

	if table, err := routes(); err == nil {
		for _, gw := range RouteGateways(table) {
			set = append(set, netip.PrefixFrom(gw, gw.BitLen()))
		}
	}

	resolve := d.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}

	for _, name := range HostDoorNames {
		lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		addrs, err := resolve(lctx, name)
		cancel()

		if err != nil {
			continue
		}

		for _, a := range addrs {
			a = a.Unmap()

			bits := 24
			if a.Is6() {
				bits = 64
			}

			if p, err := a.Prefix(bits); err == nil {
				set = append(set, p)
			}
		}
	}

	d.set.Store(&set)
}

// Refuse reports an address in the refused set. Before the first Refresh it refuses everything: a
// filter that has not yet looked must not be the one that opens the door.
func (d *Doors) Refuse(a netip.Addr) bool {
	set := d.set.Load()
	if set == nil {
		return true
	}

	return contains(*set, a.Unmap())
}

// Prefixes returns the refused set, for the filter's start-up log.
func (d *Doors) Prefixes() []netip.Prefix {
	if set := d.set.Load(); set != nil {
		return *set
	}

	return nil
}

// RouteGateways returns every gateway in a /proc/net/route table - on a container, the default
// route's next hop, which is the docker host.
//
// The file is hex in host byte order; every docker host sbx supports is little-endian.
func RouteGateways(table string) []netip.Addr {
	var out []netip.Addr

	for i, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 3 {
			continue // the header, or a blank line
		}

		b, err := hex.DecodeString(f[2])
		if err != nil || len(b) != 4 {
			continue
		}

		var a [4]byte

		binary.BigEndian.PutUint32(a[:], binary.LittleEndian.Uint32(b))

		if gw := netip.AddrFrom4(a); !gw.IsUnspecified() {
			out = append(out, gw)
		}
	}

	return out
}
