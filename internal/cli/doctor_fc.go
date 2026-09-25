package cli

import (
	"os"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
)

// firecrackerCapabilities is what `--provider firecracker` can do here, from the same hostcap
// decision the provider itself acts on - so doctor can never say yes to a machine the provider
// then refuses, or the reverse.
func firecrackerCapabilities(r hostcap.Report, ipForward string) []Capability {
	d := hostcap.Decide(r)

	fcCap := Capability{
		Name:   "firecracker",
		Have:   d.Backend == hostcap.Direct,
		Detail: string(d.Backend) + ": " + d.Reason,
	}

	switch d.Backend {
	case hostcap.Direct:
	case hostcap.HelperVM:
		fcCap.Meaning = "--provider firecracker runs through a Linux helper VM here, which the " +
			"helper-VM layer provisions; this machine cannot run it directly"
	case hostcap.KataRuntimeClass:
		fcCap.Meaning = "use --provider kubernetes --isolation kata for a VM boundary"
	default:
		fcCap.Meaning = "--provider firecracker is refused: " + d.Next
	}

	caps := []Capability{fcCap}

	// Only where firecracker would run here. On a Mac the helper VM's own doctor answers this.
	if r.OS != "linux" {
		return caps
	}

	mkfs := Capability{Name: "mkfs.ext4", Have: r.Mkfs != "", Detail: r.Mkfs,
		Meaning: "the firecracker provider cannot build a root filesystem; install e2fsprogs"}
	if r.Mkfs == "" {
		mkfs.Detail = "not on PATH"
	}

	caps = append(caps, mkfs)

	fwd := strings.TrimSpace(ipForward)
	switch fwd {
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

	return caps
}

func readIPForward() string {
	b, _ := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	return string(b)
}
