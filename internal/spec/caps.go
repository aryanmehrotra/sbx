package spec

import (
	"fmt"
	"strings"
)

// linuxCapabilities is every capability name the Linux kernel defines (capability.h, 0
// CAP_CHOWN through 40 CAP_CHECKPOINT_RESTORE), without the CAP_ prefix, as SPEC.md spells them.
//
// A list checked here rather than left to docker, because docker only checks it at create -
// after the image pull, in a message naming "CAP_NOT_A_CAP", a string the spec never contained -
// and `sbx validate` passing a spec that create then refuses is the one thing it exists to
// prevent. The cost is that a capability a future kernel adds is refused until it is added here.
// That is one line, and it fails loudly with the name in it rather than quietly.
var linuxCapabilities = map[string]bool{
	"CHOWN": true, "DAC_OVERRIDE": true, "DAC_READ_SEARCH": true, "FOWNER": true, "FSETID": true,
	"KILL": true, "SETGID": true, "SETUID": true, "SETPCAP": true, "LINUX_IMMUTABLE": true,
	"NET_BIND_SERVICE": true, "NET_BROADCAST": true, "NET_ADMIN": true, "NET_RAW": true,
	"IPC_LOCK": true, "IPC_OWNER": true, "SYS_MODULE": true, "SYS_RAWIO": true, "SYS_CHROOT": true,
	"SYS_PTRACE": true, "SYS_PACCT": true, "SYS_ADMIN": true, "SYS_BOOT": true, "SYS_NICE": true,
	"SYS_RESOURCE": true, "SYS_TIME": true, "SYS_TTY_CONFIG": true, "MKNOD": true, "LEASE": true,
	"AUDIT_WRITE": true, "AUDIT_CONTROL": true, "SETFCAP": true, "MAC_OVERRIDE": true,
	"MAC_ADMIN": true, "SYSLOG": true, "WAKE_ALARM": true, "BLOCK_SUSPEND": true, "AUDIT_READ": true,
	"PERFMON": true, "BPF": true, "CHECKPOINT_RESTORE": true,
}

// checkCapAdd refuses a cap_add entry docker would refuse at create, and ALL.
//
// Case-insensitive, and the CAP_ prefix is stripped rather than refused, because docker accepts
// both spellings and a spec written either way created on v0.15: refusing a spelling that works
// breaks a committed file for the sake of a style preference. The docker provider passes the
// name through as written, which docker normalizes the same way.
//
// ALL is refused although docker accepts it. It grants every capability (CapEff
// 000001ffffffffff), which is the capability half of `privileged` - the option this project
// deliberately does not have (see the CapAdd field). A spec that needs a lot should still say
// what, so the reviewer of the committed file can see it.
//
// A blank entry is refused: it names nothing, so it is a typo or an empty template variable,
// and the docker provider silently dropping it would hide which.
//
// Every bad entry is named in one error, as env's unset variables are (resolveEnv): stopping at
// the first made ["NOT_A_CAP", "ALSO_BAD", ""] three failed validates to find out.
func checkCapAdd(name string, caps []string) error {
	var bad []string

	for _, raw := range caps {
		c := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(raw)), "CAP_")

		switch {
		case c == "":
			bad = append(bad, "cap_add has a blank entry - remove it, or name the capability, like \"SYS_PTRACE\"")
		case c == "ALL":
			bad = append(bad, fmt.Sprintf("cap_add %q grants every capability, and sbx has no privileged "+
				"option - list the specific capabilities the workload needs, like [\"SYS_PTRACE\", \"NET_ADMIN\"] "+
				"(`man 7 capabilities`)", raw))
		case linuxCapabilities[c]:
			continue
		default:
			bad = append(bad, fmt.Sprintf("cap_add %q is not a Linux capability - use a name from "+
				"`man 7 capabilities`, like \"SYS_PTRACE\"", raw))
		}
	}

	switch len(bad) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("service %q: %s", name, bad[0])
	default:
		return fmt.Errorf("service %q: %d cap_add entries are refused:\n  - %s", name, len(bad),
			strings.Join(bad, "\n  - "))
	}
}
