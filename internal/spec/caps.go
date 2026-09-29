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

// checkCapAdd refuses a cap_add entry docker would refuse at create.
//
// Case-insensitive, and "ALL" allowed, because docker accepts both and a spec that creates
// today must keep loading. The CAP_ prefix is refused although docker would strip it: SPEC.md
// has one spelling, and a committed file that mixes two is one a reviewer has to decode.
func checkCapAdd(name string, caps []string) error {
	for _, raw := range caps {
		c := strings.ToUpper(strings.TrimSpace(raw))

		switch {
		case c == "":
			// The docker provider skips a blank entry, so it is harmless; refusing it now
			// would break a spec that works.
			continue
		case c == "ALL" || linuxCapabilities[c]:
			continue
		case strings.HasPrefix(c, "CAP_") && linuxCapabilities[strings.TrimPrefix(c, "CAP_")]:
			return fmt.Errorf("service %q: cap_add %q: write it without the CAP_ prefix, as %q",
				name, raw, strings.TrimPrefix(c, "CAP_"))
		default:
			return fmt.Errorf("service %q: cap_add %q is not a Linux capability - use the name "+
				"from `man 7 capabilities` without CAP_, like \"SYS_PTRACE\"", name, raw)
		}
	}

	return nil
}
