package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// firecrackerCapabilities are the rows under the one "microVM" row: what a Direct host still
// needs for `--provider firecracker` to work. They are shown only when kind - the same
// fchost.HostBackend decision the microVM row, the provider and the redirect act on - says this
// machine runs Firecracker itself. On a helper-VM host those checks belong to the VM, whose
// provisioning installs e2fsprogs and whose bridges are its own; showing the Mac's here would
// grade a machine that never runs the VMM.
func firecrackerCapabilities(kind hostcap.Backend, mkfs, ipForward string, usage *provider.FirecrackerUsage) []Capability {
	if kind != hostcap.Direct {
		return nil
	}

	row := Capability{Name: "mkfs.ext4", Have: mkfs != "", Detail: mkfs,
		Meaning: "the firecracker provider cannot build a root filesystem; install e2fsprogs"}
	if mkfs == "" {
		row.Detail = "not on PATH"
	}

	caps := []Capability{row}

	switch strings.TrimSpace(ipForward) {
	case "0":
		caps = append(caps, Capability{Name: "vm bridges isolated", Have: true,
			Detail: "ip_forward=0: the host routes nothing between sandbox bridges"})
	case "1":
		// Not a failure - docker turns it on and sets the FORWARD policy to DROP - but the
		// isolation between two firecracker sandboxes now rests on that policy, not on sbx.
		caps = append(caps, Capability{Name: "vm bridges isolated", Have: false,
			Detail: "ip_forward=1",
			Meaning: "sandbox bridges are separated by the host's FORWARD policy (docker sets it " +
				"to DROP), not by sbx; check `iptables -S FORWARD` if nothing else manages it"})
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

func readIPForward() string {
	b, _ := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	return string(b)
}
