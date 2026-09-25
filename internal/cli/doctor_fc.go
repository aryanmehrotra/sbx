package cli

import (
	"fmt"

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

	// Checked, not assumed: sbx writes no firewall rule, so whether one sandbox's VMs can reach
	// another's is the host's FORWARD policy, which sbx neither sets nor owns.
	if iso.Known {
		caps = append(caps, Capability{Name: "vm bridges isolated", Have: iso.Isolated,
			Detail: iso.Detail, Meaning: iso.Meaning})
	}

	// What sleeping VMs cost: each one's memory file is as big as its RAM, and nothing else in
	// doctor would show a fleet of them filling the disk.
	if usage != nil {
		caps = append(caps, Capability{Name: "microVM disk", Have: true,
			Detail: fmt.Sprintf("%s in %d VMs under %s (memory %s, disks %s, snapshots %s)",
				bytesIEC(usage.Total()), usage.VMs, usage.Root, bytesIEC(usage.Memory),
				bytesIEC(usage.Disks), bytesIEC(usage.Snapshots))})
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
