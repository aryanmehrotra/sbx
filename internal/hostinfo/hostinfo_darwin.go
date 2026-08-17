package hostinfo

// macOS keeps both figures behind tools rather than files: sysctl for the size of the machine,
// vm_stat for what is going on inside it. Two forks per refresh is the price, and it is small
// beside the round trip to the container runtime that happens on the same tick.

import (
	"os/exec"
	"strconv"
	"strings"
)

func memory() (total, free uint64) {
	total = sysctlUint("hw.memsize")

	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return total, 0
	}

	// "Mach Virtual Memory Statistics: (page size of 16384 bytes)"
	page := uint64(4096)
	if i := strings.Index(string(out), "page size of "); i >= 0 {
		if n, err := strconv.ParseUint(strings.Fields(string(out)[i+13:])[0], 10, 64); err == nil {
			page = n
		}
	}

	// Available rather than merely unused. macOS spends idle memory on cache it hands back the
	// moment something asks for it, so counting only "Pages free" reports a machine with 32 GB
	// and nothing running as nearly full - which is alarming and wrong.
	var pages uint64

	for _, line := range strings.Split(string(out), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		switch strings.TrimSpace(name) {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			n, err := strconv.ParseUint(strings.Trim(strings.TrimSpace(value), "."), 10, 64)
			if err == nil {
				pages += n
			}
		}
	}

	return total, pages * page
}

func sysctlUint(name string) uint64 {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return 0
	}

	n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}

	return n
}
