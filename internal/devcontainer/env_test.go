package devcontainer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// A devcontainer's env values were copied into the spec as written, so any `${` in them - its
// own ${localEnv:X}, or a literal a program expects - produced a sandbox.json that load then
// refused. ${localEnv:X} is the host's X, which is exactly what sbx's ${X} means; everything else
// sbx cannot evaluate, so it is escaped to reach the container as written, and the import says so.
func TestDevcontainerEnvReferencesSurviveTheSpecLoad(t *testing.T) {
	t.Setenv("DB_PASSWORD", "s3cret")
	t.Setenv("HOST_USER", "ann")

	dir := t.TempDir()
	write(t, dir, ".devcontainer/devcontainer.json", `{
  "name": "api",
  "image": "alpine:3",
  "forwardPorts": [8080],
  "workspaceFolder": "/work",
  "containerEnv": {
    "PW": "${localEnv:DB_PASSWORD}",
    "WITH_DEFAULT": "${localEnv:HOST_USER:nobody}",
    "WS": "${containerWorkspaceFolder}/bin",
    "TEMPLATE": "hello ${name}, ${X:-y}",
    "UNCLOSED": "a${b",
    "ALREADY": "$${literal}",
    "PLAIN": "pa$word"
  },
  "remoteEnv": { "PATH": "${containerEnv:PATH}:/extra" }
}`)

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	sp := spec.Spec{Version: 1, Services: map[string]spec.Service{got.Service: got.Spec}}
	sp.Services[got.Service] = func(s spec.Service) spec.Service { s.Mounts = nil; return s }(got.Spec)

	body, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := spec.ParseSpec(body, "sandbox.json")
	if err != nil {
		t.Fatalf("the imported spec does not load: %v\nenv: %v", err, got.Spec.Env)
	}

	env := loaded.Services[got.Service].Env
	for k, want := range map[string]string{
		"PW":           "s3cret",
		"WITH_DEFAULT": "ann",
		"WS":           "/work/bin",
		"TEMPLATE":     "hello ${name}, ${X:-y}",
		"UNCLOSED":     "a${b",
		"ALREADY":      "$${literal}",
		"PLAIN":        "pa$word",
		"PATH":         "${containerEnv:PATH}:/extra",
	} {
		if env[k] != want {
			t.Errorf("%s reached the container as %q, want %q (spec had %q)", k, env[k], want, got.Spec.Env[k])
		}
	}

	dropped := strings.Join(got.Dropped, "\n")
	for _, want := range []string{"HOST_USER", "nobody", "${containerEnv:PATH}"} {
		if !strings.Contains(dropped, want) {
			t.Errorf("the import does not say what it could not honour (%s):\n%s", want, dropped)
		}
	}
}

// Text that only looks like a reference is escaped silently: a note saying "${X:-y} is a
// devcontainer variable" would be wrong.
func TestALiteralThatIsNotADevcontainerVariableIsNotReported(t *testing.T) {
	var notes []string

	got := translateEnv("T", "${X:-y} ${name}", "/w", func(m string) { notes = append(notes, m) })
	if got != "$${X:-y} $${name}" || len(notes) != 0 {
		t.Errorf("got %q, notes %v; want both escaped and nothing reported", got, notes)
	}
}

// Whatever a devcontainer's env value is, the translated one must be a value sandbox.json accepts:
// a refused import is the bug this exists for. And a value with no ${ is never touched.
func FuzzTranslateEnvLoads(f *testing.F) {
	for _, s := range []string{"${localEnv:A}", "${localEnv:A:d}", "${containerEnv:P}", "${", "$${x}",
		"a${b", "${X:-y}", "${}", "${localEnv:}", "${localEnv:1x}", "$$${", "pa$word", "${${0}"} {
		f.Add(s)
	}

	dir := f.TempDir()

	f.Fuzz(func(t *testing.T, val string) {
		got := translateEnv("K", val, "/w", func(string) {})
		if !strings.Contains(val, "${") && got != val {
			t.Fatalf("%q with no ${ was changed to %q", val, got)
		}

		body, err := json.Marshal(map[string]any{"version": 1, "services": map[string]any{
			"a": map[string]any{"image": "x", "ports": []int{1}, "env": map[string]string{"K": got}}}})
		if err != nil {
			t.Fatal(err)
		}

		path := filepath.Join(dir, "sandbox.json")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := spec.LoadSpecUnexpanded(path); err != nil && strings.Contains(err.Error(), "does not expand") {
			t.Fatalf("%q translated to %q, which load refuses: %v", val, got, err)
		}

		// With nothing sbx evaluates in it, the container must get exactly the devcontainer's text.
		// Invalid UTF-8 is replaced by the JSON encoding itself, not by anything here.
		if !utf8.ValidString(val) || strings.Contains(val, "localEnv:") || strings.Contains(val, "containerWorkspaceFolder") {
			return
		}

		sp, err := spec.ParseSpec(body, "sandbox.json")
		if err != nil {
			return // refused for something other than env syntax, e.g. invalid UTF-8
		}

		if v := sp.Services["a"].Env["K"]; v != val {
			t.Fatalf("%q reached the container as %q (spec had %q)", val, v, got)
		}
	})
}
