package fc

import (
	"os"
	"os/exec"
	"strings"
)

// BridgeIsolation is whether this host drops traffic routed from one sandbox's bridge to
// another's by its own policy. A bridge whose guard is installed drops everything forwarded from
// or to it itself (fc.Guard's mangle FORWARD rules); this is what isolates the rest: with
// ip_forward off nothing is routed at all; with it on (docker turns it on) the FORWARD chain's
// policy decides, and docker sets it to DROP - but sbx neither sets nor owns that policy, so it
// checks it instead of assuming it.
type BridgeIsolation struct {
	Known    bool   // there was something to check: Linux, where firecracker runs
	Isolated bool   // confirmed: nothing is routed between sandbox bridges
	Detail   string // what was read
	Meaning  string // when not confirmed, what that means and what to do
}

// CheckBridgeIsolation decides from ip_forward's value and a reader of `iptables -S FORWARD`.
func CheckBridgeIsolation(ipForward string, forwardRules func() (string, error)) BridgeIsolation {
	switch strings.TrimSpace(ipForward) {
	case "0":
		return BridgeIsolation{Known: true, Isolated: true,
			Detail: "ip_forward=0: the host routes nothing between sandbox bridges"}
	case "1":
	default:
		return BridgeIsolation{}
	}

	rules, err := forwardRules()
	if err != nil {
		return BridgeIsolation{Known: true, Detail: "ip_forward=1; the FORWARD policy could not be read (" +
			strings.TrimSpace(err.Error()) + ")",
			Meaning: "isolation between microVM sandboxes rests on the host's FORWARD policy being DROP, and " +
				"this could not confirm it - check `sudo iptables -S FORWARD` shows `-P FORWARD DROP`"}
	}

	for _, line := range strings.Split(rules, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "-P" && f[1] == "FORWARD" {
			if f[2] == "DROP" {
				return BridgeIsolation{Known: true, Isolated: true, Detail: "ip_forward=1, FORWARD policy DROP"}
			}

			return BridgeIsolation{Known: true, Detail: "ip_forward=1, FORWARD policy " + f[2],
				Meaning: "the host routes between sandbox bridges, so one microVM sandbox can reach " +
					"another's services - `sudo iptables -P FORWARD DROP` (what docker sets), or " +
					"`sysctl net.ipv4.ip_forward=0` on a host that runs no containers"}
		}
	}

	return BridgeIsolation{Known: true, Detail: "ip_forward=1; no FORWARD policy line in `iptables -S FORWARD`",
		Meaning: "could not confirm the FORWARD policy is DROP; check `sudo iptables -S FORWARD`"}
}

// HostBridgeIsolation reads this host. Read-only: it runs `iptables -S`, never a rule change.
func HostBridgeIsolation() BridgeIsolation {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return BridgeIsolation{}
	}

	return CheckBridgeIsolation(string(b), func() (string, error) {
		out, err := exec.Command("iptables", "-S", "FORWARD").CombinedOutput()
		if err != nil {
			return "", &readErr{strings.TrimSpace(string(out)), err}
		}

		return string(out), nil
	})
}

type readErr struct {
	out string
	err error
}

func (e *readErr) Error() string {
	if e.out != "" {
		return e.out
	}

	return e.err.Error()
}
