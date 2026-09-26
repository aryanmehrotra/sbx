package cli

import (
	"fmt"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// firecrackerCapabilities are the rows under the one "microVM" row: what a Direct host still
// needs for `--provider firecracker` to work. They are shown only when kind - the same
// fchost.HostBackend decision the microVM row, the provider and the redirect act on - says this
// machine runs Firecracker itself. On a helper-VM host those checks belong to the VM, whose
// provisioning installs e2fsprogs and whose bridges are its own; showing the Mac's here would
// grade a machine that never runs the VMM.
func firecrackerCapabilities(kind hostcap.Backend, mkfs string, iso fc.BridgeIsolation, usage *provider.FirecrackerUsage) []Capability {
	if kind != hostcap.Direct {
		return nil
	}

	row := Capability{Name: "mkfs.ext4", Have: mkfs != "", Detail: mkfs,
		Meaning: "the firecracker provider cannot build a root filesystem; install e2fsprogs"}
	if mkfs == "" {
		row.Detail = "not on PATH"
	}

	caps := []Capability{row}

	// The host's FORWARD policy: what isolates sandboxes from each other on a bridge whose guard
	// is not installed. A guarded bridge drops everything forwarded from or to it on its own (the
	// mangle FORWARD rules, fc.Guard); firecrackerGuardRows says how many are guarded.
	if iso.Known {
		caps = append(caps, Capability{Name: "vm bridges isolated", Have: iso.Isolated,
			Detail: iso.Detail, Meaning: iso.Meaning})
	}

	// No disk quota per VM (SECURITY.md): on /, one sandbox - or a compromised VMM writing in its jail
	// as its own uid - can fill the host's disk. Before the disk row, which tests read as the last.
	if usage != nil && usage.SharesRootFS {
		caps = append(caps, Capability{Name: "microVM state filesystem", Have: false,
			Detail: usage.Root + " is on the same filesystem as /",
			Meaning: "sbx sets no per-VM disk quota, so one sandbox's disk, snapshots or jail can fill / (ENOSPC) " +
				"for the host and every other sandbox; put SBX_FC_STATE on a filesystem of its own, or one with " +
				"project quotas - SECURITY.md"})
	}

	// What sleeping VMs cost: each one's memory file is as big as its RAM, and nothing else in
	// doctor would show a fleet of them filling the disk.
	if usage != nil {
		detail := fmt.Sprintf("%s in %d VMs under %s (memory %s, disks %s, snapshots %s, volumes %s",
			bytesIEC(usage.Total()), usage.VMs, usage.Root, bytesIEC(usage.Memory),
			bytesIEC(usage.Disks), bytesIEC(usage.Snapshots), bytesIEC(usage.Volumes))

		// Only when there is any: a jail normally holds nothing of its own.
		if usage.Jails > 0 {
			detail += ", jails " + bytesIEC(usage.Jails)
		}

		detail += ")"

		// A warm pool that waits asleep trades RAM for exactly this: say how much of it is the pool's.
		if usage.PoolMembers > 0 {
			detail += fmt.Sprintf("; %d parked warm-pool members hold %s of it", usage.PoolMembers,
				bytesIEC(usage.PoolMemory))
		}

		caps = append(caps, Capability{Name: "microVM disk", Have: true, Detail: detail})
	}

	return caps
}

func bytesIEC(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KiB", n>>10)
	}
}

// firecrackerGuardRows are the rows that say whether the host is actually closed to its guests:
// iptables present at all, and how many of this host's sbxfc<slot> bridges have their guard in
// place (fc.CountGuards, which checks and writes nothing). Direct hosts only, like the rows above.
func firecrackerGuardRows(kind hostcap.Backend, iptables error, c fc.GuardCount, countErr error) []Capability {
	if kind != hostcap.Direct {
		return nil
	}

	ipt := Capability{Name: "iptables", Have: iptables == nil, Detail: "on PATH",
		Meaning: "sbx cannot close the host to a microVM's guests, so every microVM create and wake is " +
			"refused (fail closed). Install iptables, or set " + fc.FirewallEnv + "=unmanaged if this host's own firewall closes it to 10.231.0.0/16"}
	if iptables != nil {
		ipt.Detail = iptables.Error()
	}

	rows := []Capability{ipt}

	if iptables != nil {
		return rows
	}

	g := Capability{Name: "vm bridges guarded", Have: true,
		Detail: fmt.Sprintf("%d/%d", c.Guarded, c.Total)}

	switch {
	case countErr != nil:
		g.Have = false
		g.Detail = "could not check: " + countErr.Error()
		g.Meaning = "reading the firewall needs root: run `sudo sbx doctor`"
	case c.Guarded < c.Total:
		g.Have = false
		g.Detail += " - unguarded: " + strings.Join(c.Unguarded, ", ")

		if len(c.IPv6On) > 0 {
			g.Detail += " (IPv6 not disabled on " + strings.Join(c.IPv6On, ", ") + ": guests reach [::] services over link-local)"
		}

		g.Meaning = "those bridges' guests can reach the host and docker-published ports; the daemon " +
			"puts a guard back on its next reconcile, and the log says why it could not if it cannot"
	case c.Total == 0:
		g.Detail = "no microVM bridges on this host"
	}

	return append(rows, g)
}

// firecrackerGuards are the two controls between a guest and this host: the host guard, which
// fails closed (a VM it cannot install is refused), and the jailer, which confines the VMM.
// guard is fc.Available; cgroup2 finds the unified hierarchy the jailer's limits go in.
func firecrackerGuards(getenv func(string) string, guard func() error, cgroup2 func() (string, bool)) []Capability {
	var caps []Capability

	g := Capability{Name: "vm host guard"}

	switch mode, err := fc.FirewallFromEnv(getenv); {
	case err != nil:
		g.Detail, g.Meaning = err.Error(), "every microVM create fails until "+fc.FirewallEnv+" is managed or unmanaged"
	case mode == fc.FirewallUnmanaged:
		g.Have, g.Detail = true, "unmanaged ("+fc.FirewallEnv+"): sbx writes no rule; this host's own firewall must "+
			"close it to 10.231.0.0/16 (SECURITY.md)"
	default:
		if gerr := guard(); gerr != nil {
			g.Detail = "managed, but " + gerr.Error()
			g.Meaning = "every microVM create and wake is refused (fail closed): install iptables, or set " +
				fc.FirewallEnv + "=unmanaged (sbx serve --fc-firewall=unmanaged) if this host's firewall closes it"
		} else {
			g.Have, g.Detail = true, "managed: each bridge's guard is installed and verified, or its VM is refused"
		}
	}

	caps = append(caps, g)

	j := Capability{Name: "vm jailer"}

	switch jail, err := fc.JailFromEnv(getenv); {
	case err != nil:
		j.Detail, j.Meaning = err.Error(), "every microVM create fails until "+fc.JailerEnv+" is on or off"
	case jail == nil:
		j.Detail = fc.JailerEnv + "=off"
		j.Meaning = "every VMM runs as root, unconfined: a guest that escapes into it has this host (SECURITY.md)"
	default:
		first := fmt.Sprintf("uid %d+", jail.UIDBase)
		if mnt, ok := cgroup2(); ok {
			j.Have, j.Detail = true, "on: each VMM chrooted in its VM's directory as its own "+first+
				", limited by cgroup v2 at "+mnt
		} else {
			j.Detail = "on, but no cgroup v2 hierarchy is mounted"
			j.Meaning = "every microVM create fails in the jailer; mount cgroup2, or for a development host " +
				"only set " + fc.JailerEnv + "=off"
		}
	}

	return append(caps, j)
}
