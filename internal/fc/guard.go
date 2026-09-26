package fc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Guard closes the host to a sandbox's guests, except for the one door they are meant to have.
//
// A guest's bridge has no NAT, so nothing routed leaves - but the host itself is ON the bridge,
// at 10.231.<slot>.1, and a service the host binds to 0.0.0.0 (sshd, a database, the docker API
// on tcp) answers a guest there. The host's INPUT chain is the only thing that decides that, and
// sbx wrote nothing to it, so it was the operator's to close (SECURITY.md).
//
// Now sbx closes it for the bridges it owns, and only those. Each bridge gets its own chain,
// SBX-FC<slot>, in two tables, each reached by rules at the top of that table's built-in chains
// that match the bridge by name:
//
//	mangle:
//	-A SBX-FC<slot> -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN   replies
//	-A SBX-FC<slot> -p tcp -d 10.231.<slot>.1 --dport <filter> -j RETURN   the egress filter
//	-A SBX-FC<slot> -j DROP                                                everything else
//	-I PREROUTING 1 -i sbxfc<slot> -j SBX-FC<slot>
//	-I FORWARD 1 -i sbxfc<slot> -j DROP
//	-I FORWARD 1 -o sbxfc<slot> -j DROP
//	filter:
//	-A SBX-FC<slot> -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN   replies to the host
//	-A SBX-FC<slot> -p tcp -d 10.231.<slot>.1 --dport <filter> -j ACCEPT   the egress filter
//	-A SBX-FC<slot> -j DROP                                                everything else
//	-I INPUT 1 -i sbxfc<slot> -j SBX-FC<slot>
//
// INPUT alone is not enough. Docker's nat PREROUTING DNATs every packet addressed to a local
// address (addrtype LOCAL) and a published port onto the container behind it, so a guest dialling
// 10.231.<slot>.1:<published port> becomes FORWARD traffic that DOCKER's filter chain accepts -
// and never reaches INPUT. A container's own IP and a kube-proxy NodePort are the same story. The
// mangle chain runs after conntrack and before nat, so it drops what a guest starts before DNAT
// can rewrite it; docker writes nothing to mangle, so a docker restart cannot reorder it. The
// FORWARD drops say the rest: a guest's bridge has no NAT and no route, the host reaches its
// guests as OUTPUT, and nothing is ever meant to be forwarded from or to it.
//
// Replies RETURN rather than ACCEPT, so the host's own rules still decide the traffic it
// started (the wake proxy dialling a guest). The filter port is ACCEPTed in filter, so a host
// whose INPUT policy is DROP still lets a guest reach its filter. Nothing outside those names is
// read, written or reordered, and all of it goes away with the bridge (Release), so a host with
// no microVM sandbox has no rule of sbx's at all.
//
// It is made when the bridge is made, and checked (Ensure: `-C` only, no write while it is whole)
// on every wake and every daemon reconcile, so a rule flushed by hand, or by a firewall reload,
// is put back within one refresh interval rather than staying gone until the sandbox is recreated.
//
// IPv6 is closed by disabling it on the bridge: a guest kernel brings up an fe80:: address on its
// own, and the host's bridge would answer it with every service bound to [::].
type Guard struct {
	// Run executes iptables with args. A field so tests see the commands without a netns.
	Run func(ctx context.Context, args ...string) (string, error)

	// Sysctl writes value to a /proc/sys path. A field for the same reason.
	Sysctl func(path, value string) error

	// Port is the one the guests may reach on their gateway: the egress filter's.
	Port int

	mu sync.Mutex
}

// ErrNoFirewall is a host without iptables: nothing can be installed, and the warning stands.
var ErrNoFirewall = errors.New("iptables is not on PATH")

// NewGuard runs the real iptables, waiting for the xtables lock rather than failing on it.
func NewGuard(port int) *Guard {
	return &Guard{
		Port: port,
		Run: func(ctx context.Context, args ...string) (string, error) {
			bin, err := exec.LookPath("iptables")
			if err != nil {
				return "", ErrNoFirewall
			}

			out, err := exec.CommandContext(ctx, bin, append([]string{"-w"}, args...)...).CombinedOutput()
			if err != nil {
				return string(out), fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}

			return string(out), nil
		},
		Sysctl: func(path, value string) error { return os.WriteFile(path, []byte(value), 0o644) },
	}
}

// Chain is the per-bridge chain's name. The same name in each table the guard writes to: tables
// have separate namespaces, and one name makes `iptables-save | grep SBX-FC5` find all of it.
func (a Addr) Chain() string { return "SBX-FC" + strconv.Itoa(a.Slot) }

// share is one table's part of the guard: a chain of the guard's own, and the rules at the top of
// that table's built-in chains that send the bridge's traffic to it (or drop it outright).
type share struct {
	table string     // "" is filter
	rules [][]string // the chain's contents, in order
	hooks [][]string // {built-in chain, rule...}, each inserted at position 1
}

// shares is the whole guard for a, built only from a's integers and the filter port.
func (g *Guard) shares(a Addr) []share {
	br, chain, gw, port := a.Bridge(), a.Chain(), a.Gateway(), strconv.Itoa(g.Port)

	return []share{
		{
			// Before nat: docker's PREROUTING DNATs a guest's packet for 10.231.<slot>.1:<a
			// published port> onto a container, after which it is FORWARD traffic that DOCKER
			// accepts and INPUT never sees. Mangle runs after conntrack (so a reply to the host's
			// own dial is known as one) and before nat, and docker writes nothing to it.
			table: "mangle",
			rules: [][]string{
				{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"},
				{"-p", "tcp", "-d", gw, "--dport", port, "-j", "RETURN"},
				{"-j", "DROP"},
			},
			hooks: [][]string{
				// A guest has no route anywhere, and nothing is routed to it: the bridge has no
				// NAT and is reached only by the host itself, which is OUTPUT, not FORWARD.
				{"FORWARD", "-o", br, "-j", "DROP"},
				{"FORWARD", "-i", br, "-j", "DROP"},
				{"PREROUTING", "-i", br, "-j", chain},
			},
		},
		{
			table: "",
			rules: [][]string{
				{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"},
				{"-p", "tcp", "-d", gw, "--dport", port, "-j", "ACCEPT"},
				{"-j", "DROP"},
			},
			hooks: [][]string{{"INPUT", "-i", br, "-j", chain}},
		},
	}
}

// ipt runs iptables against table ("" is filter).
func (g *Guard) ipt(ctx context.Context, table string, args ...string) error {
	if table != "" {
		args = append([]string{"-t", table}, args...)
	}

	_, err := g.Run(ctx, args...)

	return err
}

// NoIPv6 turns IPv6 off on the bridge. Called before the bridge is up, so its link-local
// address is never assigned. A kernel built without IPv6 has nothing to turn off.
func (g *Guard) NoIPv6(a Addr) error {
	err := g.Sysctl("/proc/sys/net/ipv6/conf/"+a.Bridge()+"/disable_ipv6", "1")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}

// Install makes the bridge's chains and the rules that reach them, in every table. Idempotent.
// On any failure it removes what it made, so the host is left either guarded or exactly as it
// was - never with half a guard.
func (g *Guard) Install(ctx context.Context, a Addr) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.install(ctx, a)
}

func (g *Guard) install(ctx context.Context, a Addr) error {
	chain := a.Chain()
	shares := g.shares(a)

	// Every chain filled before anything jumps to one: a jump to a chain still being written
	// would be a moment with the rules half there.
	for _, s := range shares {
		if err := g.ipt(ctx, s.table, "-N", chain); err != nil {
			if errors.Is(err, ErrNoFirewall) {
				return err
			}

			// Left by a bridge deleted without sbx: reuse the name, rebuild the contents.
			if lerr := g.ipt(ctx, s.table, "-n", "-L", chain); lerr != nil {
				return errors.Join(err, g.release(ctx, a))
			}

			// Already whole - a repair after a hook went missing: left alone, because flushing a
			// chain something still jumps to would open the bridge for as long as it is empty.
			if g.holds(ctx, s, chain) {
				continue
			}
		}

		if err := g.ipt(ctx, s.table, "-F", chain); err != nil {
			return errors.Join(err, g.release(ctx, a))
		}

		for _, r := range s.rules {
			if err := g.ipt(ctx, s.table, append([]string{"-A", chain}, r...)...); err != nil {
				return errors.Join(err, g.release(ctx, a))
			}
		}
	}

	for _, s := range shares {
		for _, h := range s.hooks {
			if g.ipt(ctx, s.table, append([]string{"-C"}, h...)...) == nil {
				continue
			}

			if err := g.ipt(ctx, s.table, append([]string{"-I", h[0], "1"}, h[1:]...)...); err != nil {
				return errors.Join(err, g.release(ctx, a))
			}
		}
	}

	return nil
}

// Release removes the bridge's rules and chains from every table. Idempotent: a bridge that
// never had them, or whose rules were already removed, is success.
func (g *Guard) Release(ctx context.Context, a Addr) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.release(ctx, a)
}

func (g *Guard) release(ctx context.Context, a Addr) error {
	chain := a.Chain()

	var errs []error

	for _, s := range g.shares(a) {
		for _, h := range s.hooks {
			// Every copy, in case two processes inserted one each.
			for range 16 {
				if err := g.ipt(ctx, s.table, append([]string{"-D"}, h...)...); err != nil {
					if errors.Is(err, ErrNoFirewall) {
						return nil // nothing could ever have been installed
					}

					break
				}
			}
		}

		if g.ipt(ctx, s.table, "-n", "-L", chain) != nil {
			continue // no chain: nothing to remove
		}

		if err := g.ipt(ctx, s.table, "-F", chain); err != nil {
			errs = append(errs, err)
			continue
		}

		if err := g.ipt(ctx, s.table, "-X", chain); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Available is nil where Install can run: iptables is on PATH.
func Available() error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return ErrNoFirewall
	}

	return nil
}

// holds reports whether chain in s's table has every rule s gives it.
func (g *Guard) holds(ctx context.Context, s share, chain string) bool {
	for _, r := range s.rules {
		if g.ipt(ctx, s.table, append([]string{"-C", chain}, r...)...) != nil {
			return false
		}
	}

	return true
}

// Ensure puts back a guard something removed while its bridge stood - a `iptables -F`, a
// firewall reload, a docker restart that flushed mangle - and is otherwise a handful of `-C`
// checks and no write. Called on every wake and every daemon reconcile, so a rule flushed by hand
// is gone for one refresh interval at most, not until the sandbox is recreated.
//
// What it checks is what a flush removes: each hook, and each chain's final DROP. A rule edited
// out of the middle of a chain by hand is not looked for; an `iptables -F` of either table is.
func (g *Guard) Ensure(ctx context.Context, a Addr) (repaired bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	chain := a.Chain()
	whole := true

	for _, s := range g.shares(a) {
		checks := [][]string{{"-C", chain, "-j", "DROP"}}
		for _, h := range s.hooks {
			checks = append(checks, append([]string{"-C"}, h...))
		}

		for _, c := range checks {
			if err := g.ipt(ctx, s.table, c...); err != nil {
				if errors.Is(err, ErrNoFirewall) {
					return false, err
				}

				whole = false

				break
			}
		}

		if !whole {
			break
		}
	}

	if whole {
		return false, nil
	}

	return true, g.install(ctx, a)
}
