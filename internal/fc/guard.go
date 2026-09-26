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
// SBX-FC<slot>, reached by one jump at the top of INPUT that matches that bridge by name:
//
//	-A SBX-FC<slot> -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN   replies to the host
//	-A SBX-FC<slot> -p tcp -d 10.231.<slot>.1 --dport <filter> -j ACCEPT   the egress filter
//	-A SBX-FC<slot> -j DROP                                                everything else
//	-I INPUT 1 -i sbxfc<slot> -j SBX-FC<slot>
//
// Replies RETURN rather than ACCEPT, so the host's own rules still decide the traffic it
// started (the wake proxy dialling a guest). The filter port is ACCEPTed, so a host whose INPUT
// policy is DROP still lets a guest reach its filter. Nothing outside those two names is read,
// written or reordered, and the pair goes away with the bridge (Release), so a host with no
// microVM sandbox has no rule of sbx's at all.
//
// It is made when the bridge is made, not on every wake: a wake is on the latency path, and the
// bridge and its chain have the same life - a reboot takes both, and the next wake remakes both.
// A rule flushed by hand while the bridge exists stays gone until the sandbox is next recreated.
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

// Chain is the per-bridge chain's name.
func (a Addr) Chain() string { return "SBX-FC" + strconv.Itoa(a.Slot) }

func (g *Guard) jump(a Addr) []string { return []string{"INPUT", "-i", a.Bridge(), "-j", a.Chain()} }

// NoIPv6 turns IPv6 off on the bridge. Called before the bridge is up, so its link-local
// address is never assigned. A kernel built without IPv6 has nothing to turn off.
func (g *Guard) NoIPv6(a Addr) error {
	err := g.Sysctl("/proc/sys/net/ipv6/conf/"+a.Bridge()+"/disable_ipv6", "1")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}

// Install makes the bridge's chain and its jump. Idempotent. On any failure it removes what it
// made, so the host is left either guarded or exactly as it was - never with half a chain.
func (g *Guard) Install(ctx context.Context, a Addr) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	chain := a.Chain()

	if _, err := g.Run(ctx, "-N", chain); err != nil {
		if errors.Is(err, ErrNoFirewall) {
			return err
		}

		// Left by a bridge deleted without sbx: reuse the name, rebuild the contents.
		if _, lerr := g.Run(ctx, "-n", "-L", chain); lerr != nil {
			return err
		}
	}

	// Filled before anything jumps to it: a jump to a chain still being written would be a
	// moment with the rules half there.
	for _, rule := range [][]string{
		{"-F", chain},
		{"-A", chain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"},
		{"-A", chain, "-p", "tcp", "-d", a.Gateway(), "--dport", strconv.Itoa(g.Port), "-j", "ACCEPT"},
		{"-A", chain, "-j", "DROP"},
	} {
		if _, err := g.Run(ctx, rule...); err != nil {
			return errors.Join(err, g.release(ctx, a))
		}
	}

	if _, err := g.Run(ctx, append([]string{"-C"}, g.jump(a)...)...); err == nil {
		return nil
	}

	if _, err := g.Run(ctx, append([]string{"-I", "INPUT", "1"}, g.jump(a)[1:]...)...); err != nil {
		return errors.Join(err, g.release(ctx, a))
	}

	return nil
}

// Release removes the bridge's jump and chain. Idempotent: a bridge that never had them, or
// whose rules were already removed, is success.
func (g *Guard) Release(ctx context.Context, a Addr) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.release(ctx, a)
}

func (g *Guard) release(ctx context.Context, a Addr) error {
	// Every copy of the jump, in case two processes inserted one each.
	for range 16 {
		if _, err := g.Run(ctx, append([]string{"-D"}, g.jump(a)...)...); err != nil {
			if errors.Is(err, ErrNoFirewall) {
				return nil // nothing could ever have been installed
			}

			break
		}
	}

	chain := a.Chain()

	if _, err := g.Run(ctx, "-n", "-L", chain); err != nil {
		return nil // no chain: nothing to remove
	}

	if _, err := g.Run(ctx, "-F", chain); err != nil {
		return err
	}

	_, err := g.Run(ctx, "-X", chain)

	return err
}

// Available is nil where Install can run: iptables is on PATH.
func Available() error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return ErrNoFirewall
	}

	return nil
}
