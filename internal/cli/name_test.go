package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxNames(t *testing.T) {
	ok := []string{"main", "feature-x", "a", "release-2026.08", "agent_1", "x9", "A-B"}
	bad := []string{
		"",                  // nothing to name
		"feature/login",     // what a branch is actually called — the common mistake
		"../../../tmp/evil", // and why the rule is written down rather than inherited
		"-leading-dash",
		".hidden",
		"has space",
		"quote\"d",
		strings.Repeat("x", 101),
	}

	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) rejected a usable name: %v", n, err)
		}
	}

	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) accepted an unusable name", n)
		}
	}
}

// The rule is also what keeps an origin record inside its directory. Stated as a test so
// that loosening the name rule fails here rather than quietly writing somewhere else.
func TestAValidNameCannotEscapeTheOriginsDirectory(t *testing.T) {
	base := "/home/u/.sbx/origins"

	for _, n := range []string{"main", "feature-x", "release-2026.08", "a.b.c", "x_1"} {
		if err := ValidateName(n); err != nil {
			t.Fatalf("%q should be valid: %v", n, err)
		}

		got := filepath.Clean(filepath.Join(base, n+".json"))
		if filepath.Dir(got) != base {
			t.Errorf("a valid name resolved outside the origins directory: %s", got)
		}
	}
}
