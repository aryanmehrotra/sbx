package features

import (
	"strings"
	"testing"
)

// SBX_FEATURES is typed by hand and pasted into CI files, so its parser takes whatever a person
// or a shell produces. It must never panic and must never turn something on that was not named.
func FuzzFeatureList(f *testing.F) {
	for _, s := range []string{
		"ssh", "ssh,devcontainer", " ssh , devcontainer ", "", ",", ",,,",
		"all", "*", "true", "ssh=1", "ssh:on", "SSH", "ssh ssh", strings.Repeat("a,", 500),
		"\x00", "ssh\n", "ssh\tdevcontainer", "-ssh", "!ssh",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, list string) {
		got := parse(list)

		for name := range got {
			// Nothing empty, and nothing carrying the separator or the padding the parser is
			// supposed to have removed - those would never match a registered name, so a match
			// would mean the parser had invented one.
			if name == "" {
				t.Errorf("parse(%q) produced an empty name", list)
			}

			if strings.Contains(name, ",") {
				t.Errorf("parse(%q) kept a separator inside %q", list, name)
			}

			if name != strings.TrimSpace(name) {
				t.Errorf("parse(%q) kept padding around %q", list, name)
			}

			// Every name it produces has to appear in the input. A parser that can conjure a
			// name can turn a feature on that nobody asked for, which is the only outcome here
			// that actually matters.
			if !strings.Contains(list, name) {
				t.Errorf("parse(%q) produced %q, which is not in the input", list, name)
			}
		}

		// The one hard rule: a feature is on only if it was named. Whatever the input, a name
		// that does not appear cannot be enabled.
		if got["ssh"] && !strings.Contains(list, "ssh") {
			t.Errorf("parse(%q) enabled ssh without it being named", list)
		}
	})
}
