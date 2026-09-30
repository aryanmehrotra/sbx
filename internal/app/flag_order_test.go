package app

import (
	"strings"
	"testing"
)

// `sbx logs -f sb svc` is how people type it - `tail -f file`, `docker logs -f name` - and it
// printed a usage error, because only LEADING positionals were taken and a leading flag hid
// them all. Flags go before, between or after the names.
//
// An invalid --isolation makes each call fail in resolve, after argument parsing and before
// anything touches a provider, so reaching that error is the proof that parsing found the
// sandbox name - and the test never follows a real log.
func TestLogsAndEnvTakeFlagsBeforeOrAfterTheNames(t *testing.T) {
	cases := []struct {
		cmd  string
		args []string
	}{
		{"logs", []string{"-f", "--isolation", "bogus", "sb", "svc"}},
		{"logs", []string{"sb", "-f", "svc", "--isolation", "bogus"}},
		{"logs", []string{"sb", "svc", "-f", "--tail", "5", "--isolation", "bogus"}},
		{"logs", []string{"--tail", "5", "--isolation=bogus", "sb"}},
		{"env", []string{"--shell", "json", "--isolation", "bogus", "sb"}},
		{"env", []string{"sb", "--shell", "json", "--isolation", "bogus"}},
	}

	for _, c := range cases {
		err := dispatch(c.cmd, c.args)
		if err == nil || !strings.Contains(err.Error(), "unknown isolation") {
			t.Errorf("sbx %s %s: got %v, want parsing to find the sandbox and reach resolve",
				c.cmd, strings.Join(c.args, " "), err)
		}
	}
}

// Everything after a bare -- is a name, even one that looks like a flag.
func TestParsePositionalHonoursDoubleDash(t *testing.T) {
	fs := newFlagSet("t")
	follow := fs.Bool("f", false, "")

	got := parsePositional(fs, []string{"-f", "sb", "--", "-weird", "-f"})

	if strings.Join(got, "|") != "sb|-weird|-f" || !*follow {
		t.Errorf("positionals %q, follow=%v; want [sb -weird -f] and -f set once", got, *follow)
	}
}
