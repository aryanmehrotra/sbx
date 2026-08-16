package ui

import (
	"strings"
	"testing"
)

// Real lines, copied from real containers. A log formatter tested against invented input is
// tested against the author's idea of a log rather than against a log.
func TestFormattingRealLogLines(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantTime string
		keep     []string // must survive: a formatter that eats information is worse than none
		gone     []string // the fixed parts that are the same on every line and say nothing
	}{
		{
			name:     "mysql warning",
			raw:      "2026-08-16T12:14:03.671056Z 0 [Warning] [MY-011068] [Server] The syntax '--skip-host-cache' is deprecated",
			wantTime: "12:14:03",
			keep:     []string{"[Warning]", "MY-011068", "--skip-host-cache", "deprecated"},
		},
		{
			name:     "mysql entrypoint, no fractional seconds",
			raw:      "2026-08-16 11:55:58+00:00 [Note] [Entrypoint]: Entrypoint script for MySQL Server 8.0.46-1.el9 started.",
			wantTime: "11:55:58",
			keep:     []string{"Entrypoint", "8.0.46"},
			gone:     []string{"+00:00"},
		},
		{
			name:     "redis notice",
			raw:      "1:M 15 Aug 2026 18:48:36.295 * Ready to accept connections tcp",
			wantTime: "18:48:36",
			keep:     []string{"1:M", "Ready to accept connections"},
		},
		{
			name:     "redis warning",
			raw:      "1:C 15 Aug 2026 18:48:36.294 # Warning: no config file specified",
			wantTime: "18:48:36",
			keep:     []string{"Warning", "no config file"},
		},
		{
			name:     "postgres",
			raw:      "2026-08-16 12:14:03.671 UTC [1] LOG:  database system is ready to accept connections",
			wantTime: "12:14:03",
			keep:     []string{"LOG", "ready to accept connections"},
		},
		{
			name:     "nginx access",
			raw:      `127.0.0.1 - - [16/Aug/2026:12:14:03 +0000] "GET / HTTP/1.1" 200 612`,
			wantTime: "",
			keep:     []string{"GET /", "200"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripColour(formatLog(c.raw))

			if c.wantTime != "" && !strings.HasPrefix(got, c.wantTime) {
				t.Errorf("does not lead with the time %q:\n  %s", c.wantTime, got)
			}

			for _, k := range c.keep {
				if !strings.Contains(got, k) {
					t.Errorf("formatting lost %q:\n  in:  %s\n  out: %s", k, c.raw, got)
				}
			}

			for _, g := range c.gone {
				if strings.Contains(got, g) {
					t.Errorf("the timezone offset was left behind: %s", got)
				}
			}

			// The date is dropped from the front, because every line has it and it is today.
			if c.wantTime != "" && strings.HasPrefix(got, "2026-") {
				t.Errorf("still leads with a full date: %s", got)
			}
		})
	}
}

// A line it does not recognise must come through untouched. This is output somebody is
// relying on to debug, and mangling it is the one unforgivable thing here.
func TestUnrecognisedLinesPassThrough(t *testing.T) {
	for _, raw := range []string{
		"'/var/lib/mysql/mysql.sock' -> '/var/run/mysqld/mysqld.sock'",
		"some entirely unstructured output",
		"",
		"1:signal-handler (1786820242) Received SIGTERM scheduling shutdown...",
	} {
		if got := stripColour(formatLog(raw)); got != raw {
			t.Errorf("changed an unrecognised line:\n  in:  %q\n  out: %q", raw, got)
		}
	}
}

// A warning should be findable without reading, and an ordinary line should not shout.
func TestLevelsAreColoured(t *testing.T) {
	warn := formatLog("2026-08-16T12:14:03.671056Z 0 [Warning] [MY-011068] [Server] deprecated")
	if !strings.Contains(warn, yellow) {
		t.Error("a warning is not coloured, so it cannot be found at a glance")
	}

	errLine := formatLog("2026-08-16 12:14:03.671 UTC [1] ERROR:  relation does not exist")
	if !strings.Contains(errLine, red) {
		t.Error("an error is not coloured")
	}

	// The classic false positive: a line that merely mentions the word.
	note := formatLog("2026-08-16 12:14:03.671 UTC [1] LOG:  no error was found in the table")
	if strings.Contains(note, red) {
		t.Error("a LOG line mentioning 'error' was coloured as an error")
	}
}
