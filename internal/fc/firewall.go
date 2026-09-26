package fc

import (
	"fmt"
	"strings"
)

// FirewallMode is who closes the host to its microVM guests.
type FirewallMode string

const (
	// FirewallManaged (the default): sbx installs and verifies a Guard per bridge, and a VM whose
	// bridge's guard cannot be installed or verified - iptables missing, a rule refused, a repair
	// that fails - does not start. Fail closed: a guest must never find the host open because a
	// firewall module was missing.
	FirewallManaged FirewallMode = "managed"

	// FirewallUnmanaged: the operator's own firewall closes the host (INPUT, and anything that
	// DNATs a guest's packet past it), and sbx writes no rule at all. The operator's
	// responsibility, stated in SECURITY.md; `sbx serve --fc-firewall=unmanaged`.
	FirewallUnmanaged FirewallMode = "unmanaged"

	// FirewallEnv carries the mode to every sbx process: SBX_FC_FIREWALL.
	FirewallEnv = "SBX_FC_FIREWALL"
)

// FirewallFromEnv reads SBX_FC_FIREWALL. A value it does not know is an error, not either mode.
func FirewallFromEnv(getenv func(string) string) (FirewallMode, error) {
	switch v := FirewallMode(strings.ToLower(strings.TrimSpace(getenv(FirewallEnv)))); v {
	case "", FirewallManaged:
		return FirewallManaged, nil
	case FirewallUnmanaged:
		return FirewallUnmanaged, nil
	default:
		return "", fmt.Errorf("%s=%q: managed (the default) or unmanaged", FirewallEnv, string(v))
	}
}
