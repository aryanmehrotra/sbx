package provider

import "testing"

// CopyVolume asserts what it moved rather than assuming it. Docker creates an empty volume
// for a name that does not exist instead of failing, so a copy from a snapshot that was never
// made is otherwise a silent success producing nothing at all - which is the exact shape
// DECISIONS.md records for the docker-commit bug: a working server and an empty database.
func TestParseCopyCount(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		from, to int
		ok       bool
	}{
		{"normal", "SBXCOUNT 24 24", 24, 24, true},
		{"with other output", "pulling alpine\nSBXCOUNT 3 3\n", 3, 3, true},
		{"empty source", "SBXCOUNT 0 0", 0, 0, true},

		// No marker at all: the script did not get far enough to report, which must be an
		// error rather than a zero-file success.
		{"no marker", "cp: cannot stat", 0, 0, false},
		{"truncated", "SBXCOUNT 24", 0, 0, false},
		{"not numbers", "SBXCOUNT a b", 0, 0, false},
		{"nothing", "", 0, 0, false},
	}

	for _, c := range cases {
		from, to, ok := parseCopyCount(c.out)
		if ok != c.ok || from != c.from || to != c.to {
			t.Errorf("%s: parseCopyCount(%q) = (%d, %d, %v), want (%d, %d, %v)",
				c.name, c.out, from, to, ok, c.from, c.to, c.ok)
		}
	}
}
