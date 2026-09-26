package fc

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// Networking: a tap per VM on a bridge per sandbox, addressed from the slot.
//
// The model is the one `egress: "deny"` already uses for docker (DECISIONS.md, "Egress is denied
// by a bridge without NAT"): sbx writes no iptables rule, so nothing masquerades traffic from
// these bridges and nothing routed leaves the host. The host reaches every guest directly on its
// bridge address, which is all the wake proxy needs - it dials the guest's IP and port, exactly
// as it dials a container's backing port.
//
// One bridge per sandbox rather than one for the machine, so two sandboxes do not share a
// layer-2 segment. Between bridges the host routes only if ip_forward is on and the FORWARD
// policy lets it; docker turns forwarding on and sets that policy to DROP, and sbx does not
// write a rule of its own to make sure - `sbx doctor` reports ip_forward so it is visible.
//
// Addresses are arithmetic, never allocated: 10.231.<slot>.0/24, the bridge at .1, a service at
// .<its port index + 2>. A slot is already unique per sandbox and an index per service within
// it, so two processes computing the address of the same service always agree - which matters
// because Create, the daemon's Start and a restore after a host reboot may each be a different
// sbx process.

// Plan is every address the arithmetic below can produce: each sandbox's bridge gateway (the
// host) and every guest. Nothing else is ever in it, so a filter on the host can refuse it whole.
var Plan = netip.MustParsePrefix("10.231.0.0/16")

// Addr is one VM's place on the network.
type Addr struct {
	Slot  int // the sandbox's port block, 0..59
	Index int // the service's first port index within the block, 0..19
}

func (a Addr) Bridge() string  { return fmt.Sprintf("sbxfc%d", a.Slot) }
func (a Addr) Tap() string     { return fmt.Sprintf("sbxfc%d-%d", a.Slot, a.Index) } // <= 15 bytes, IFNAMSIZ
func (a Addr) Gateway() string { return fmt.Sprintf("10.231.%d.1", a.Slot) }
func (a Addr) GuestIP() string { return fmt.Sprintf("10.231.%d.%d", a.Slot, a.Index+2) }
func (a Addr) MAC() string     { return fmt.Sprintf("06:00:0a:e7:%02x:%02x", a.Slot, a.Index+2) }

// BootArg is the kernel's own IP autoconfiguration (CONFIG_IP_PNP=y in the pinned kernel):
// eth0 comes up with its address before PID 1 runs, so no image needs `ip` or a DHCP client.
func (a Addr) BootArg() string {
	return fmt.Sprintf("ip=%s::%s:255.255.255.0::eth0:off", a.GuestIP(), a.Gateway())
}

// Valid is whether the arithmetic above stays inside one byte per octet and IFNAMSIZ.
func (a Addr) Valid() error {
	if a.Slot < 0 || a.Slot > 253 || a.Index < 0 || a.Index > 250 {
		return fmt.Errorf("slot %d index %d is outside the 10.231.0.0/16 plan", a.Slot, a.Index)
	}

	return nil
}

// Network creates and removes the host side.
type Network interface {
	EnsureTap(ctx context.Context, a Addr) error
	RemoveTap(ctx context.Context, a Addr) error
	RemoveBridge(ctx context.Context, slot int) error
}

// IPNetwork is Network over iproute2's `ip`, shelled out for the same reason mkfs.ext4 is: it
// exists, it is what an operator would type, and netlink by hand is a lot of code to get subtly
// wrong with no dependencies. It needs CAP_NET_ADMIN.
type IPNetwork struct {
	// Run executes `ip` with args. A field so tests see the commands without a netns.
	Run func(ctx context.Context, args ...string) (string, error)

	// Owner is the uid firecracker runs as, given to the tap so a non-root firecracker can open
	// it. -1 leaves the tap root-owned.
	Owner int

	// Guard closes the host to the bridge's guests except for the egress filter's port; nil
	// leaves the host's INPUT chain to its operator, as it always was. Warn is told when it
	// could not be installed, which leaves the bridge working and the host as open as before.
	Guard *Guard
	Warn  func(string)
}

// NewIPNetwork runs the real `ip`.
func NewIPNetwork(owner int) *IPNetwork {
	return &IPNetwork{Owner: owner, Run: func(ctx context.Context, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				return "", errors.New("`ip` (iproute2) is not on PATH: the firecracker provider " +
					"creates each VM's tap and bridge with it. Install iproute2")
			}

			return string(out), fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}

		return string(out), nil
	}}
}

func (n *IPNetwork) exists(ctx context.Context, dev string) bool {
	_, err := n.Run(ctx, "link", "show", "dev", dev)
	return err == nil
}

// EnsureTap makes the sandbox's bridge and this VM's tap exist and be up. Idempotent: the
// wake path calls it on every Start, because a host reboot takes every tap with it and a
// snapshot restores a VM that expects its tap to be there.
func (n *IPNetwork) EnsureTap(ctx context.Context, a Addr) error {
	if err := a.Valid(); err != nil {
		return err
	}

	br, tap := a.Bridge(), a.Tap()

	if !n.exists(ctx, br) {
		if _, err := n.Run(ctx, "link", "add", br, "type", "bridge"); err != nil && !n.exists(ctx, br) {
			return fmt.Errorf("creating bridge %s: %w", br, err)
		}

		n.guard(ctx, a)

		for _, args := range [][]string{
			{"addr", "add", a.Gateway() + "/24", "dev", br},
			{"link", "set", br, "up"},
		} {
			if _, err := n.Run(ctx, args...); err != nil && !n.exists(ctx, br) {
				return fmt.Errorf("creating bridge %s: %w", br, err)
			}
		}
	} else {
		n.recheck(ctx, a)
	}

	if !n.exists(ctx, tap) {
		args := []string{"tuntap", "add", "dev", tap, "mode", "tap"}
		if n.Owner >= 0 {
			args = append(args, "user", fmt.Sprint(n.Owner))
		}

		if _, err := n.Run(ctx, args...); err != nil {
			return fmt.Errorf("creating tap %s: %w", tap, err)
		}
	}

	for _, args := range [][]string{
		{"link", "set", tap, "master", br},
		{"link", "set", tap, "up"},
	} {
		if _, err := n.Run(ctx, args...); err != nil {
			return fmt.Errorf("attaching tap %s to %s: %w", tap, br, err)
		}
	}

	return nil
}

// guard closes the new bridge to the host before it is up, so there is no moment when a guest
// could reach the host through it. A failure is reported and the bridge used anyway: the host is
// then exactly as open as it was before sbx guarded anything, which SECURITY.md describes, and a
// sandbox that stopped booting because a firewall module was missing would be a regression.
func (n *IPNetwork) guard(ctx context.Context, a Addr) {
	if n.Guard == nil {
		return
	}

	warn := func(format string, args ...any) {
		if n.Warn != nil {
			n.Warn(fmt.Sprintf(format, args...))
		}
	}

	if err := n.Guard.NoIPv6(a); err != nil {
		warn("could not turn IPv6 off on %s, so its guests may reach host services bound to [::] "+
			"over link-local: %v", a.Bridge(), err)
	}

	if err := n.Guard.Install(ctx, a); err != nil {
		warn("could not close the host to %s's guests (%v): they can reach every host service "+
			"bound to 0.0.0.0 at %s - see SECURITY.md", a.Bridge(), err, a.Gateway())
	}
}

// recheck puts the guard back if something removed it while the bridge stood. A host with no
// iptables was told so when the bridge was made, and is not told again on every wake.
func (n *IPNetwork) recheck(ctx context.Context, a Addr) {
	if n.Guard == nil {
		return
	}

	repaired, err := n.Guard.Ensure(ctx, a)

	switch {
	case errors.Is(err, ErrNoFirewall):
	case err != nil:
		if n.Warn != nil {
			n.Warn(fmt.Sprintf("%s's host rules were missing and could not be put back (%v): its guests "+
				"can reach host services at %s - see SECURITY.md", a.Bridge(), err, a.Gateway()))
		}
	case repaired:
		if n.Warn != nil {
			n.Warn(fmt.Sprintf("%s's host rules had been removed (a firewall reload or flush?) and were put back",
				a.Bridge()))
		}
	}
}

// EnsureGuard is recheck for the daemon's reconcile: a bridge that exists gets its guard checked
// and, if something removed it, put back. No bridge, nothing to guard.
func (n *IPNetwork) EnsureGuard(ctx context.Context, slot int) {
	a := Addr{Slot: slot}
	if n.Guard == nil || a.Valid() != nil || !n.exists(ctx, a.Bridge()) {
		return
	}

	n.recheck(ctx, a)
}

// RemoveTap deletes the VM's tap; one already gone is success.
func (n *IPNetwork) RemoveTap(ctx context.Context, a Addr) error {
	if !n.exists(ctx, a.Tap()) {
		return nil
	}

	_, err := n.Run(ctx, "link", "del", a.Tap())

	return err
}

// RemoveBridge deletes the sandbox's bridge once nothing is on it.
func (n *IPNetwork) RemoveBridge(ctx context.Context, slot int) error {
	br := Addr{Slot: slot}.Bridge()

	// First, and whether or not the bridge is still there: a bridge deleted by hand leaves its
	// chain behind, and this is the one place that can collect it.
	if n.Guard != nil {
		if err := n.Guard.Release(ctx, Addr{Slot: slot}); err != nil {
			return fmt.Errorf("removing %s's host rules: %w", br, err)
		}
	}

	if !n.exists(ctx, br) {
		return nil
	}

	_, err := n.Run(ctx, "link", "del", br)

	return err
}
