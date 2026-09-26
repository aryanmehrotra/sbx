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

	// What sleeping VMs cost: each one's memory file is as big as its RAM, and nothing else in
	// doctor would show a fleet of them filling the disk.
	if usage != nil {
		detail := fmt.Sprintf("%s in %d VMs under %s (memory %s, disks %s, snapshots %s, volumes %s)",
			bytesIEC(usage.Total()), usage.VMs, usage.Root, bytesIEC(usage.Memory),
			bytesIEC(usage.Disks), bytesIEC(usage.Snapshots), bytesIEC(usage.Volumes))

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
		Meaning: "sbx cannot close the host to a microVM's guests: they reach every host service " +
			"bound to 0.0.0.0 at 10.231.<slot>.1, and docker-published ports. Install iptables"}
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
		g.Meaning = "those bridges' guests can reach the host and docker-published ports; the daemon " +
			"puts a guard back on its next reconcile, and the log says why it could not if it cannot"
	case c.Total == 0:
		g.Detail = "no microVM bridges on this host"
	}

	return append(rows, g)
}
