package hostcap

import (
	"os"
	"strconv"
	"strings"
)

// capNetAdmin is CAP_NET_ADMIN's bit in the capability sets (linux/capability.h).
const capNetAdmin = 12

// NetAdmin reports whether this process may create taps and bridges and write iptables rules:
// root, or CAP_NET_ADMIN in its effective set. A microVM's network needs all three.
func NetAdmin() bool {
	if os.Geteuid() == 0 {
		return true
	}

	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}

	return capEffHas(string(b), capNetAdmin)
}

// capEffHas reads the CapEff line of a /proc/<pid>/status and reports whether bit is set.
func capEffHas(status string, bit uint) bool {
	for _, line := range strings.Split(status, "\n") {
		v, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}

		n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)

		return err == nil && n&(1<<bit) != 0
	}

	return false
}
