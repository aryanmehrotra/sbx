package spec

import (
	"strings"
	"testing"
)

func capSpec(caps string) []byte {
	return []byte(`{"version":1,"services":{"a":{"image":"alpine","ports":[80],"cap_add":[` + caps + `]}}}`)
}

// An unknown capability used to pass `sbx validate` and fail only at `docker create`, after the
// image pull, with docker's own text about "CAP_NOT_A_CAP" - a name the spec never contained.
func TestCapAddRefusesAnUnknownCapability(t *testing.T) {
	_, err := ParseSpec(capSpec(`"SYS_PTRACE","NOT_A_CAP"`), "spec.json")
	if err == nil {
		t.Fatal("an unknown capability was accepted")
	}

	for _, want := range []string{"spec.json", `service "a"`, "cap_add", `"NOT_A_CAP"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// SPEC.md spells capabilities without the prefix. Saying so beats "unknown capability", which
// would read as though CAP_SYS_PTRACE did not exist.
func TestCapAddRefusesTheCapPrefixWithAHint(t *testing.T) {
	_, err := ParseSpec(capSpec(`"CAP_SYS_PTRACE"`), "spec.json")
	if err == nil {
		t.Fatal("a CAP_-prefixed capability was accepted")
	}

	if !strings.Contains(err.Error(), `"SYS_PTRACE"`) || !strings.Contains(err.Error(), "CAP_") {
		t.Errorf("error %q should suggest the unprefixed name", err)
	}
}

func TestCapAddAcceptsKnownCapabilities(t *testing.T) {
	// Lower case and ALL are what docker itself accepts, so refusing them would break a spec
	// that creates today.
	for _, caps := range []string{
		`"SYS_PTRACE"`, `"CHECKPOINT_RESTORE"`, `"NET_ADMIN","SYS_ADMIN"`, `"sys_ptrace"`,
		`"ALL"`, `"BPF","PERFMON"`,
	} {
		if _, err := ParseSpec(capSpec(caps), "spec.json"); err != nil {
			t.Errorf("cap_add [%s] was refused: %v", caps, err)
		}
	}
}
