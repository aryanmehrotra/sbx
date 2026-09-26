package fc

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Networking: a tap per VM on a bridge per sandbox, addressed from the slot.
//
// The model is the one `egress: "deny"` already uses for docker (DECISIONS.md, "Egress is denied
// by a bridge without NAT"): nothing masquerades traffic from these bridges and nothing routed
// leaves the host. The only rules sbx writes are the guard's (guard.go), which close the host and
// drop forwarding for the bridges sbx owns. The host reaches every guest directly on its
// bridge address, which is all the wake proxy needs - it dials the guest's IP and port, exactly
// as it dials a container's backing port.
//
// One bridge per sandbox rather than one for the machine, so two sandboxes do not share a
// layer-2 segment. Between bridges the guard's mangle FORWARD drops keep them apart; where the
// guard could not be installed the host routes only if ip_forward is on and the FORWARD policy
// lets it - `sbx doctor` reports both.
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

	// RecheckGuard verifies - and puts back - the host guard of a's bridge without touching its
	// tap: for a VM resumed in place, whose VMM already holds the tap. An error is the resume's
	// refusal (fail closed); nil when the operator owns the firewall, or the bridge is gone.
	RecheckGuard(ctx context.Context, a Addr) error

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

	// OwnerOf, when set, is the uid a VM's tap is made for instead of Owner: a jailed VMM opens
	// its tap as its own uid (JailConfig.UID). TapOwner reads an existing tap's owner, so one made
	// for another uid is made again - a tun device's owner cannot be changed.
	OwnerOf  func(Addr) int
	TapOwner func(tap string) (int, bool)

	// Guard closes the host to the bridge's guests except for the egress filter's port, and FAILS
	// CLOSED: a bridge whose guard cannot be installed, or put back, gets no VM (FirewallManaged).
	// Nil is FirewallUnmanaged - the operator's firewall owns the host and sbx writes no rule.
	// Warn is told what the daemon's reconcile could not repair on a bridge already in use.
	Guard *Guard
	Warn  func(string)

	// PerVMNetNS makes each VM's tap in a network namespace of its own (ensureNetNS), which its
	// jailed VMM joins (JailSpec.NetNS): the VMM then has no interface on the host's network.
	PerVMNetNS bool
}

// tapOwner is the uid a's tap is made for; -1 leaves it root's.
func (n *IPNetwork) tapOwner(a Addr) int {
	if n.OwnerOf != nil {
		return n.OwnerOf(a)
	}

	return n.Owner
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

		// Guarded before it is up, and removed if it cannot be: no moment when a guest could
		// reach the host through it, and nothing left for a retry to mistake for a guarded one.
		if err := n.guard(ctx, a); err != nil {
			_, _ = n.Run(ctx, "link", "del", br)
			return err
		}

		for _, args := range [][]string{
			{"addr", "add", a.Gateway() + "/24", "dev", br},
			{"link", "set", br, "up"},
		} {
			if _, err := n.Run(ctx, args...); err != nil && !n.exists(ctx, br) {
				return fmt.Errorf("creating bridge %s: %w", br, err)
			}
		}
	} else if err := n.recheck(ctx, a); err != nil {
		return err
	}

	if n.PerVMNetNS {
		return n.ensureNetNS(ctx, a)
	}

	owner := n.tapOwner(a)

	// A tap made for another uid - root's, by a VMM before the jailer - is one this VMM cannot
	// open, and a tun device's owner cannot be changed: made again.
	if owner >= 0 && n.TapOwner != nil {
		if got, ok := n.TapOwner(tap); ok && got != owner {
			if _, err := n.Run(ctx, "link", "del", tap); err != nil {
				return fmt.Errorf("re-making tap %s for uid %d: %w", tap, owner, err)
			}
		}
	}

	if !n.exists(ctx, tap) {
		args := []string{"tuntap", "add", "dev", tap, "mode", "tap"}
		if owner >= 0 {
			args = append(args, "user", fmt.Sprint(owner))
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

// refusal is why a VM on a's bridge was not started, and what to do about it.
func refusal(a Addr, what string, err error) error {
	return fmt.Errorf("refusing to start a microVM on %s: %s (%w), so its guests could reach every host "+
		"service bound to 0.0.0.0 at %s. Install iptables and see `sbx doctor`; on a host whose own "+
		"firewall closes it to 10.231.0.0/16, set %s=unmanaged (sbx serve --fc-firewall=unmanaged) - "+
		"SECURITY.md", a.Bridge(), what, err, a.Gateway(), FirewallEnv)
}

// guard closes the new bridge to the host before it is up. A failure is the VM's refusal: fail
// closed, since a guest that finds the host open because a firewall module was missing is the
// one outcome this guard exists to prevent (v0.12 warned and booted; v0.13 does not).
func (n *IPNetwork) guard(ctx context.Context, a Addr) error {
	if n.Guard == nil {
		return nil
	}

	if err := n.Guard.NoIPv6(a); err != nil {
		return refusal(a, "IPv6 could not be turned off on it, and guests would reach [::] over link-local", err)
	}

	if err := n.Guard.Install(ctx, a); err != nil {
		return refusal(a, "the host guard could not be installed", err)
	}

	return nil
}

// recheck verifies the guard of a bridge that stands, and puts it back if something removed it.
// One that cannot be verified or put back is the VM's refusal, as in guard.
func (n *IPNetwork) recheck(ctx context.Context, a Addr) error {
	if n.Guard == nil {
		return nil
	}

	repaired, err := n.Guard.Ensure(ctx, a)

	switch {
	case err != nil:
		return refusal(a, "its host guard was missing and could not be put back", err)
	case repaired && n.Warn != nil:
		n.Warn(fmt.Sprintf("%s's host rules had been removed (a firewall reload or flush?) and were put back",
			a.Bridge()))
	}

	return nil
}

// RecheckGuard is recheck for a VM resumed in place (Network.RecheckGuard): no bridge, nothing
// a guest could reach the host through.
func (n *IPNetwork) RecheckGuard(ctx context.Context, a Addr) error {
	if err := a.Valid(); err != nil {
		return err
	}

	if n.Guard == nil || !n.exists(ctx, a.Bridge()) {
		return nil
	}

	return n.recheck(ctx, a)
}

// EnsureGuard is recheck for the daemon's reconcile: a bridge that exists gets its guard checked
// and, if something removed it, put back. No bridge, nothing to guard. What it cannot put back is
// warned about; the VMs already on that bridge keep running, and no new one starts on it.
func (n *IPNetwork) EnsureGuard(ctx context.Context, slot int) {
	a := Addr{Slot: slot}
	if n.Guard == nil || a.Valid() != nil || !n.exists(ctx, a.Bridge()) {
		return
	}

	if err := n.recheck(ctx, a); err != nil && n.Warn != nil {
		n.Warn(fmt.Sprintf("%s's host rules were removed and could not be put back: its running guests "+
			"can reach host services at %s, and no VM will start on it until they are (%v)",
			a.Bridge(), a.Gateway(), err))
	}
}

// RemoveTap deletes the VM's tap; one already gone is success.
func (n *IPNetwork) RemoveTap(ctx context.Context, a Addr) error {
	// Its namespace goes, and the tap and the veth pair with it. Also when this process would not
	// make one (the jailer turned off since): a namespace left behind holds its name.
	if _, err := n.Run(ctx, "netns", "del", a.NetNS()); err != nil && n.PerVMNetNS && n.netnsExists(a) {
		return err
	}

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

// GuardWhole reports whether slot's bridge has its guard in place, writing nothing. With no Guard
// configured there is nothing sbx promised, and the answer is yes.
func (n *IPNetwork) GuardWhole(ctx context.Context, slot int) (bool, error) {
	if n.Guard == nil {
		return true, nil
	}

	return n.Guard.Whole(ctx, Addr{Slot: slot})
}

// SysTapOwner is a tun device's owner as the kernel reports it (/sys/class/net/<tap>/owner; -1
// for none), for IPNetwork.TapOwner. False when the tap or the file is not there.
func SysTapOwner(tap string) (int, bool) {
	b, err := os.ReadFile("/sys/class/net/" + tap + "/owner")
	if err != nil {
		return 0, false
	}

	n, err := strconv.Atoi(strings.TrimSpace(string(b)))

	return n, err == nil
}

// NetNS is the name of a's own network namespace, where its jailed VMM runs: its tap's name.
func (a Addr) NetNS() string { return a.Tap() }

// NetNSPath is where `ip netns` binds a's namespace, for the jailer's --netns.
func NetNSPath(a Addr) string { return "/var/run/netns/" + a.NetNS() }

// netnsBridge and netnsUplink are the bridge and the veth end inside a VM's namespace.
const (
	netnsBridge = "br0"
	netnsUplink = "eth0"
)

// ensureNetNS builds a's namespace from nothing: its tap inside, owned by the VM's uid, bridged
// there to a veth whose host end - named as a host tap was - is a port of the sandbox's bridge.
// Rebuilt at every launch (the caller has ended, or is ending, the VM's VMM, which is the only
// thing that uses it): whatever an earlier VMM, a reboot or a v0.12 host tap left is replaced, so
// the tap's owner is always this launch's uid and no two VMMs ever share a tap.
func (n *IPNetwork) ensureNetNS(ctx context.Context, a Addr) error {
	ns, host, tap, br := a.NetNS(), a.Tap(), a.Tap(), a.Bridge()

	_, _ = n.Run(ctx, "netns", "del", ns)

	if n.exists(ctx, host) {
		if _, err := n.Run(ctx, "link", "del", host); err != nil {
			return fmt.Errorf("removing %s's old host link: %w", host, err)
		}
	}

	in := func(args ...string) []string { return append([]string{"-n", ns}, args...) }

	steps := [][]string{
		{"netns", "add", ns},
		{"link", "add", host, "type", "veth", "peer", "name", netnsUplink, "netns", ns},
		{"link", "set", host, "master", br},
		{"link", "set", host, "up"},
		in("link", "add", netnsBridge, "type", "bridge"),
		in("link", "set", netnsUplink, "master", netnsBridge),
	}

	tuntap := in("tuntap", "add", "dev", tap, "mode", "tap")
	if owner := n.tapOwner(a); owner >= 0 {
		tuntap = append(tuntap, "user", fmt.Sprint(owner))
	}

	steps = append(steps, tuntap, in("link", "set", tap, "master", netnsBridge))

	// No IPv6 link-local address on anything inside, then up: an address the VMM could bind is
	// a network it could use.
	for _, dev := range []string{netnsBridge, netnsUplink, tap} {
		steps = append(steps, in("link", "set", "dev", dev, "addrgenmode", "none"), in("link", "set", dev, "up"))
	}

	for _, args := range steps {
		if _, err := n.Run(ctx, args...); err != nil {
			_, _ = n.Run(context.WithoutCancel(ctx), "netns", "del", ns)
			return fmt.Errorf("making %s's network namespace: %w", host, err)
		}
	}

	return nil
}

// netnsExists reports whether a's namespace is still bound; a field-free check so RemoveTap can
// tell "was never there" from "could not be removed".
func (n *IPNetwork) netnsExists(a Addr) bool {
	_, err := os.Stat(NetNSPath(a))
	return err == nil
}
