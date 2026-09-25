package fc

import (
	"context"
	"errors"
	"fmt"
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
		for _, args := range [][]string{
			{"link", "add", br, "type", "bridge"},
			{"addr", "add", a.Gateway() + "/24", "dev", br},
			{"link", "set", br, "up"},
		} {
			if _, err := n.Run(ctx, args...); err != nil && !n.exists(ctx, br) {
				return fmt.Errorf("creating bridge %s: %w", br, err)
			}
		}
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
	if !n.exists(ctx, br) {
		return nil
	}

	_, err := n.Run(ctx, "link", "del", br)

	return err
}
