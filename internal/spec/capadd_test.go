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
	for _, caps := range []string{`"SYS_PTRACE","NOT_A_CAP"`, `"CAP_NOT_A_CAP"`} {
		_, err := ParseSpec(capSpec(caps), "spec.json")
		if err == nil {
			t.Errorf("cap_add [%s]: an unknown capability was accepted", caps)
			continue
		}

		for _, want := range []string{"spec.json", `service "a"`, "cap_add", "NOT_A_CAP"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %s", err, want)
			}
		}
	}
}

func TestCapAddAcceptsKnownCapabilities(t *testing.T) {
	// Lower case and the CAP_ prefix are what docker itself accepts, and a spec written either
	// way created on v0.15, so refusing them would break a spec that works.
	for _, caps := range []string{
		`"SYS_PTRACE"`, `"CHECKPOINT_RESTORE"`, `"NET_ADMIN","SYS_ADMIN"`, `"sys_ptrace"`,
		`"BPF","PERFMON"`, `"CAP_NET_ADMIN"`, `"cap_sys_ptrace"`, `"CAP_SYS_PTRACE","NET_RAW"`,
	} {
		if _, err := ParseSpec(capSpec(caps), "spec.json"); err != nil {
			t.Errorf("cap_add [%s] was refused: %v", caps, err)
		}
	}
}

// ALL grants every capability (CapEff 000001ffffffffff, measured on v0.16.0-dev), which is the
// capability half of `privileged` - the option SPEC.md says sbx does not have.
func TestCapAddRefusesAll(t *testing.T) {
	for _, caps := range []string{`"ALL"`, `"all"`, `"SYS_PTRACE","All"`, `"CAP_ALL"`, `" ALL "`} {
		_, err := ParseSpec(capSpec(caps), "spec.json")
		if err == nil {
			t.Errorf("cap_add [%s] was accepted", caps)
			continue
		}

		for _, want := range []string{"spec.json", `service "a"`, "every capability", "privileged", "SYS_PTRACE"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error for [%s] = %q, does not mention %s", caps, err, want)
			}
		}
	}
}

// A blank entry names nothing; it is a typo or a templating hole, not a request.
func TestCapAddRefusesABlankEntry(t *testing.T) {
	for _, caps := range []string{`""`, `"SYS_PTRACE","  "`} {
		_, err := ParseSpec(capSpec(caps), "spec.json")
		if err == nil {
			t.Errorf("cap_add [%s] was accepted", caps)
			continue
		}

		if !strings.Contains(err.Error(), "blank") {
			t.Errorf("error for [%s] = %q, does not say the entry is blank", caps, err)
		}
	}
}
