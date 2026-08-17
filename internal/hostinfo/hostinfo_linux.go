package hostinfo

// Linux answers both from one file, which is why this is the short one.

import (
	"os"
	"strconv"
	"strings"
)

func memory() (total, free uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}

	for _, line := range strings.Split(string(b), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}

		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}

		// Both are in kB, whatever the unit column says.
		switch name {
		case "MemTotal":
			total = n * 1024
		case "MemAvailable":
			// MemAvailable rather than MemFree: the kernel's own estimate of what a new
			// allocation could have, which counts the cache it would drop to provide it.
			free = n * 1024
		}
	}

	return total, free
}
