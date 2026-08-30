package cli

import (
	"regexp"
	"strings"
	"testing"
)

// A sandbox name becomes a container name, a volume name and part of a DNS label, so what this
// accepts is what docker is asked to accept a moment later. Anything it lets through that the
// runtime then rejects is an error reported from the wrong place, in the runtime's vocabulary
// rather than sbx's.
func FuzzValidateName(f *testing.F) {
	for _, s := range []string{
		"feature-x", "a", "A", "0", "my_branch", "my.branch", "a-b_c.d",
		"", "-lead", ".lead", "_lead", "feature/x", "up per", "emoji-🙂",
		strings.Repeat("a", 100), strings.Repeat("a", 101),
		"a\nb", "a\tb", "a\x00b", "--", "..",
	} {
		f.Add(s)
	}

	// What a container name is allowed to be, which is the thing this gatekeeps.
	docker := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

	f.Fuzz(func(t *testing.T, name string) {
		if err := ValidateName("sandbox", name); err != nil {
			return // refusing is always safe
		}

		if name == "" {
			t.Fatal("accepted an empty name")
		}

		if len(name) > 100 {
			t.Errorf("accepted a %d-character name; the derived container names stop being "+
				"legal: %q", len(name), name)
		}

		// The whole point: `sbx-<sandbox>-<service>` has to be a container docker will take.
		if !docker.MatchString(name) {
			t.Errorf("accepted %q, which docker will reject as part of a container name", name)
		}

		for _, r := range name {
			if r == '/' || r == 0 || r == '\n' || r == '\t' || r == ' ' {
				t.Errorf("accepted %q containing %q - a name with this in it cannot be a "+
					"container name, and a slash is the branch-name case the message calls out",
					name, string(r))
			}
		}
	})
}

// A snapshot name becomes an IMAGE tag, and the rule really is different - docker container
// names allow uppercase and image repositories do not. `sbx snapshot db GOLDEN` reaching the
// runtime is what this exists to stop.
func FuzzValidateSnapshotName(f *testing.F) {
	for _, s := range []string{
		"golden", "golden-1", "v1.2.3", "a_b",
		"", "GOLDEN", "Golden", "-lead", "a/b", "a b", strings.Repeat("x", 200),
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		if err := ValidateSnapshotName(name); err != nil {
			return
		}

		if name == "" {
			t.Fatal("accepted an empty snapshot name")
		}

		if name != strings.ToLower(name) {
			t.Errorf("accepted %q with uppercase; a docker image repository cannot hold it "+
				"and the failure surfaces from the runtime instead of here", name)
		}

		if strings.ContainsAny(name, "/ \t\n") {
			t.Errorf("accepted %q, which cannot be part of an image reference", name)
		}
	})
}
