package app

// Help drifts. It is the part of a program nobody recompiles to check, and the first thing a
// new user reads - `sbx init` described itself as printing a spec to stdout for a while after
// it had become an interactive prompt.
//
// These tests make the two halves check each other: whatever the top-level help lists must
// have a per-command entry, and whatever has an entry must be a real command.

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// listedCommands are the `sbx <name>` lines in the top-level help.
func listedCommands(t *testing.T) map[string]bool {
	t.Helper()

	var buf bytes.Buffer

	usage(&buf)

	re := regexp.MustCompile(`(?m)^\s{2}sbx ([a-z]+)`)

	out := map[string]bool{}

	for _, m := range re.FindAllStringSubmatch(buf.String(), -1) {
		out[m[1]] = true
	}

	if len(out) < 15 {
		t.Fatalf("only found %d commands in the help, so this test is not reading it right", len(out))
	}

	return out
}

func TestEveryListedCommandExplainsItself(t *testing.T) {
	for name := range listedCommands(t) {
		h, ok := help[name]
		if !ok {
			t.Errorf("`sbx %s` is in the help but `sbx %s --help` has nothing to say", name, name)
			continue
		}

		if !strings.HasPrefix(h.synopsis, "sbx "+name) {
			t.Errorf("%s's synopsis is %q, which does not start with the command", name, h.synopsis)
		}

		if h.about == "" {
			t.Errorf("%s has no description", name)
		}

		if !strings.HasPrefix(h.example, "sbx ") && !strings.Contains(h.example, "sbx "+name) {
			t.Errorf("%s's example does not run sbx: %q", name, h.example)
		}
	}
}

func TestEveryExplainedCommandIsReal(t *testing.T) {
	listed := listedCommands(t)

	for name := range help {
		if !listed[name] {
			t.Errorf("`sbx %s --help` explains a command the top-level help never mentions, "+
				"so nobody will find it", name)
		}
	}
}

// dispatch must recognise everything advertised. A command in the help that falls through to
// "unknown command" is worse than one that is missing.
func TestEveryListedCommandIsDispatched(t *testing.T) {
	body := dispatchSource(t)

	for name := range listedCommands(t) {
		if !strings.Contains(body, `case "`+name+`"`) {
			t.Errorf("`sbx %s` is advertised but dispatch has no case for it", name)
		}
	}
}

func TestTheExampleMatchesTheSynopsis(t *testing.T) {
	for name, h := range help {
		// A synopsis promising a positional argument, with an example that has none, is how
		// somebody ends up typing `sbx create` and getting an error.
		if !strings.Contains(h.synopsis, "<sandbox>") {
			continue
		}

		rest := strings.TrimSpace(strings.TrimPrefix(h.example, "sbx "+name))
		if rest == "" || strings.HasPrefix(rest, "-") {
			t.Errorf("%s takes a sandbox name but its example does not give one: %q", name, h.example)
		}
	}
}

// dispatchSource reads app.go so the test can check which cases exist. Reading the source is
// crude, and the alternative - a registry the switch is generated from - is a larger change
// than the drift it prevents is worth.
func dispatchSource(t *testing.T) string {
	t.Helper()

	body, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}
