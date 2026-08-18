package app

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"
)

// scripted builds a wizard reading canned answers, which is the only reliable way to test
// this: driving it through a pty echoes the input back and the test ends up asserting against
// the terminal's behaviour rather than the program's.
func scripted(answers ...string) (*wizard, *bytes.Buffer) {
	var out bytes.Buffer

	in := ""
	for _, a := range answers {
		in += a + "\n"
	}

	return &wizard{in: bufio.NewReader(strings.NewReader(in)), out: &out}, &out
}

func TestChooseAcceptsANumberOrAName(t *testing.T) {
	names := TemplateNames()
	if len(names) < 2 {
		t.Skip("needs at least two templates")
	}

	t.Run("by number", func(t *testing.T) {
		w, _ := scripted("2")

		got, err := w.choose()
		if err != nil {
			t.Fatal(err)
		}

		if got != names[1] {
			t.Errorf("2 chose %q, want %q", got, names[1])
		}
	})

	t.Run("by name", func(t *testing.T) {
		w, _ := scripted("browser")

		got, err := w.choose()
		if err != nil {
			t.Fatal(err)
		}

		if got != "browser" {
			t.Errorf("typing the name chose %q", got)
		}
	})

	t.Run("empty takes the default", func(t *testing.T) {
		w, _ := scripted("")

		got, err := w.choose()
		if err != nil {
			t.Fatal(err)
		}

		if got != "postgres" {
			t.Errorf("pressing enter chose %q, want the postgres default", got)
		}
	})

	t.Run("a wrong answer is corrected rather than fatal", func(t *testing.T) {
		w, out := scripted("nonsense", "nginx")

		got, err := w.choose()
		if err != nil {
			t.Fatal(err)
		}

		if got != "nginx" {
			t.Errorf("the second answer was not honoured: %q", got)
		}

		if !strings.Contains(out.String(), "is not one of them") {
			t.Error("it did not say why the first answer was rejected")
		}
	})
}

// The one that shipped broken: input running out must not be read as consent. `echo | sbx
// init` created a real sandbox because the confirm defaulted to yes and EOF took the default.
func TestInputRunningOutIsNotConsent(t *testing.T) {
	w, _ := scripted() // nothing at all

	if w.confirm("Create it now?", true) {
		t.Error("a question defaulting to yes was answered yes by a closed pipe, so sbx would " +
			"create a sandbox nobody asked for")
	}
}

func TestPressingEnterStillTakesTheDefault(t *testing.T) {
	w, _ := scripted("")

	if !w.confirm("Create it now?", true) {
		t.Error("pressing enter did not take the default, which is what a default is for")
	}
}

func TestWritesTheSpecAndSaysWhatToRunNext(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// name, then no to creating it
	w, out := scripted("my-branch", "n")

	if err := w.finish("postgres"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(defaultSpec); err != nil {
		t.Fatalf("no %s was written: %v", defaultSpec, err)
	}

	got := out.String()

	for _, want := range []string{"wrote " + defaultSpec, "sbx create my-branch", "sbx env my-branch", "sbx serve"} {
		if !strings.Contains(got, want) {
			t.Errorf("the next steps do not mention %q:\n%s", want, got)
		}
	}
}

// Overwriting somebody's spec without asking is the behaviour the original `sbx init` avoided
// by printing to stdout, and it must not come back with the guided version.
func TestAnExistingSpecIsNotReplacedWithoutConsent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	const mine = `{"version":1,"services":{"mine":{"image":"redis"}}}`

	if err := os.WriteFile(defaultSpec, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	w, out := scripted("my-branch", "n") // name, then no to replacing

	if err := w.finish("postgres"); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(defaultSpec)
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != mine {
		t.Error("it replaced a spec that was already there after being told not to")
	}

	if !strings.Contains(out.String(), "Left alone") {
		t.Errorf("it did not say the file was kept:\n%s", out.String())
	}
}

func TestBranchNamesBecomeUsableSandboxNames(t *testing.T) {
	cases := map[string]string{
		"feature/ABC-123":  "feature-abc-123",
		"main":             "main",
		"release/v1.2.3":   "release-v1-2-3",
		"Fix_Thing":        "fix_thing",
		"--weird--":        "weird",
		"user@host/branch": "user-host-branch",
	}

	for in, want := range cases {
		if got := sanitiseName(in); got != want {
			t.Errorf("sanitiseName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The scripting path is the one already in people's shells and in this project's docs.
func TestThePipedPathStillPrintsTheSpec(t *testing.T) {
	var out bytes.Buffer

	if err := printTemplate("postgres", &out); err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Errorf("`sbx init > sandbox.json` no longer prints a spec:\n%s", out.String())
	}

	if strings.Contains(out.String(), "What does this branch need") {
		t.Error("a prompt reached stdout, which would end up inside sandbox.json")
	}
}
