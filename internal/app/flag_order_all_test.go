package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every command that takes names accepts sbx's flags before, between or after them.
//
// Each form carries --isolation bogus, which fails in resolve: after argument parsing and
// before anything touches a provider. Reaching "unknown isolation" therefore proves both that
// every name was found (each command checks its count first) and that a flag on either side of
// them was read - a dropped flag would have reached the real default provider instead.
func TestEveryCommandTakesFlagsBeforeOrAfterTheNames(t *testing.T) {
	t.Setenv("SBX_FEATURES", "ssh") // ssh is gated; it has to get as far as parsing
	t.Setenv("SBX_HISTORY", filepath.Join(t.TempDir(), "h.jsonl"))

	const bad = "--isolation=bogus"

	cases := []struct {
		cmd   string
		forms [][]string
	}{
		{"create", [][]string{{bad, "sb"}, {"sb", bad}}},
		{"env", [][]string{{bad, "sb"}, {"sb", bad}}},
		{"ready", [][]string{{bad, "--timeout", "1s", "sb"}, {"sb", "--timeout", "1s", bad}}},
		{"wake", [][]string{{bad, "sb"}, {"sb", bad}}},
		{"sleep", [][]string{{bad, "sb"}, {"sb", bad}}},
		{"rm", [][]string{{bad, "sb"}, {"sb", bad}}},
		{"ssh", [][]string{{bad, "sb", "svc"}, {"sb", "svc", bad}, {"sb", bad, "svc"}}},
		{"egress", [][]string{{bad, "--deny", "x.example", "sb"}, {"sb", "svc", "--deny", "x.example", bad}}},
		{"logs", [][]string{{"-f", bad, "sb", "svc"}, {"sb", "svc", "-f", bad}, {"sb", "-f", "svc", bad}}},
		{"snapshot", [][]string{{bad, "sb", "snap"}, {"sb", "snap", bad}, {"sb", bad, "snap"}}},
		{"fork", [][]string{{bad, "snap", "sb"}, {"snap", "sb", bad}, {"snap", bad, "sb"}}},
		{"checkpoint", [][]string{{bad, "sb", "cp1"}, {"sb", "cp1", bad}, {"sb", bad, "cp1"}}},
		{"resume", [][]string{{bad, "sb", "cp1"}, {"sb", "cp1", bad}, {"sb", bad, "cp1"}}},
		{"url", [][]string{{bad, "sb", "svc"}, {"sb", "svc", bad}, {"sb", bad, "svc"}}},
		{"cp", [][]string{{bad, "sb", "svc", "./a", ":/b"}, {"sb", "svc", "./a", ":/b", bad}, {"sb", "svc", bad, "./a", ":/b"}}},
		{"exec", [][]string{{bad, "sb", "svc", "true"}, {"sb", bad, "svc", "true"}, {"sb", "svc", bad, "true"}}},
		{"add", [][]string{
			{"--image", "redis:7-alpine", "--port", "6379", bad, "sb", "svc"},
			{"sb", "svc", "--image", "redis:7-alpine", "--port", "6379", bad},
			{"sb", "--image", "redis:7-alpine", "svc", "--port", "6379", bad},
		}},
		{"with", [][]string{{bad, "sb", "--", "true"}, {"sb", bad, "--", "true"}}},
	}

	for _, c := range cases {
		for _, args := range c.forms {
			err := dispatch(c.cmd, args)
			if err == nil || !strings.Contains(err.Error(), "unknown isolation") {
				t.Errorf("sbx %s %s: got %v; want parsing to find every name and the flag",
					c.cmd, strings.Join(args, " "), err)
			}
		}
	}
}

// The commands without provider flags, each with its own proof that the name was read.
func TestNameCommandsWithoutProviderFlags(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "nope-fx3.json")
	for _, args := range [][]string{{"--template", "", missing}, {missing, "--template", ""}} {
		// The path given as a name must be the one read, not the default sandbox.json.
		if err := dispatch("validate", args); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("sbx validate %s: got %v, want it to read %s", strings.Join(args, " "), err, missing)
		}
	}

	spec := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(spec, []byte(`{"version":1,"services":{"a":{"image":"fx3.invalid/none:0","ports":[1]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	for _, args := range [][]string{{"--spec", spec, "--out", out, "zz"}, {"zz", "--spec", spec, "--out", out}} {
		// A service the spec lacks is refused by name - so the name was read, and so was --spec.
		if err := dispatch("pack", args); err == nil || !strings.Contains(err.Error(), `no service "zz"`) {
			t.Errorf("sbx pack %s: got %v, want the unknown service named", strings.Join(args, " "), err)
		}
	}

	for _, args := range [][]string{{"--spec", spec, "fx3.invalid/none:0"}, {"fx3.invalid/none:0", "--spec", spec}} {
		if err := dispatch("prewarm", args); err == nil || !strings.Contains(err.Error(), "not both") {
			t.Errorf("sbx prewarm %s: got %v, want --spec and the name both seen", strings.Join(args, " "), err)
		}
	}
}

// After the names, sbx exec and sbx add hand the rest over untouched, and a -- there is part of it.
func TestParseNamesLeavesTheCommandAlone(t *testing.T) {
	cases := []struct {
		args        []string
		names, rest string
		tty         bool
	}{
		{[]string{"-t", "sb", "pg", "psql", "-U", "app"}, "sb|pg", "psql|-U|app", true},
		{[]string{"sb", "-t", "pg", "psql", "-U", "app"}, "sb|pg", "psql|-U|app", true},
		{[]string{"sb", "pg", "--", "-weird"}, "sb|pg", "-weird", false},
		{[]string{"sb", "pg", "echo", "--", "-x"}, "sb|pg", "echo|--|-x", false},
		{[]string{"--", "-sb", "pg", "cmd"}, "-sb|pg", "cmd", false},
	}

	for _, c := range cases {
		fs := newFlagSet("t")
		tty := fs.Bool("t", false, "")

		names, rest := parseNames(fs, c.args, 2)
		if strings.Join(names, "|") != c.names || strings.Join(rest, "|") != c.rest || *tty != c.tty {
			t.Errorf("%q: names %q rest %q tty %v; want %q %q %v", c.args, names, rest, *tty, c.names, c.rest, c.tty)
		}
	}
}
