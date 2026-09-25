package cli

import (
	"os"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
)

// firecrackerCapabilities are the rows under the one "microVM" row: what a Direct host still
// needs for `--provider firecracker` to work. They are shown only when kind - the same
// fchost.HostBackend decision the microVM row, the provider and the redirect act on - says this
// machine runs Firecracker itself. On a helper-VM host those checks belong to the VM, whose
// provisioning installs e2fsprogs and whose bridges are its own; showing the Mac's here would
// grade a machine that never runs the VMM.
func firecrackerCapabilities(kind hostcap.Backend, mkfs, ipForward string) []Capability {
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

	return caps
}

func readIPForward() string {
	b, _ := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	return string(b)
}
