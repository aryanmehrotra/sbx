package spec

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func envSpec(val string) []byte {
	return []byte(`{"version":1,"services":{"a":{"image":"alpine","ports":[80],"env":{"PW":` + val + `}}}}`)
}

// `${X:-y}` matched neither the expansion nor any refusal, so the container got the literal
// string "${X:-y}" - a password nobody chose, on a database that came up looking healthy.
func TestEnvRefusesBraceSyntaxThatIsNotAPlainReference(t *testing.T) {
	t.Setenv("X", "set")

	for _, val := range []string{
		`"${X:-y}"`, `"${X-y}"`, `"${X:?no}"`, `"${}"`, `"${1X}"`, `"${X"`, `"pre-${X:-y}-post"`,
		`"${X}${Y:-z}"`,
	} {
		_, err := ParseSpec(envSpec(val), "spec.json")
		if err == nil {
			t.Errorf("env value %s was accepted", val)
			continue
		}

		for _, want := range []string{"spec.json", "a.PW", "${NAME}", "shell"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error for %s = %q, does not mention %s", val, err, want)
			}
		}
	}
}

// The refusal must not reach what SPEC.md promises is left alone: a bare $NAME (passwords
// contain dollars) and the plain ${NAME} form.
func TestEnvStillAcceptsPlainReferencesAndBareDollars(t *testing.T) {
	t.Setenv("X", "set")

	for _, val := range []string{`"${X}"`, `"pa$word"`, `"$X"`, `"a$"`, `"$${X}"`, `"cost: $5 {x}"`} {
		if _, err := ParseSpec(envSpec(val), "spec.json"); err != nil {
			t.Errorf("env value %s was refused: %v", val, err)
		}
	}
}

// The check is syntax, so it must not depend on the variable being set - the refusal is about
// the form, and saying "not set" instead would send the reader to fix the wrong thing.
func TestEnvSyntaxIsCheckedWithoutTheVariableSet(t *testing.T) {
	_, err := ParseSpec(envSpec(`"${UNSET_FX6:-y}"`), "spec.json")
	if err == nil || !strings.Contains(err.Error(), "no defaults") {
		t.Fatalf("${UNSET_FX6:-y} with the variable unset: err = %v, want the syntax refusal", err)
	}
}

// The OpenSandbox API runs Service.Validate on a caller's env, which sbx never expands, so a
// literal `${X:-y}` there is a value and must not become a 400.
func TestEnvSyntaxIsNotAppliedToTheAPIsLiteralEnv(t *testing.T) {
	svc := Service{Image: "alpine", Ports: []int{80}, Env: map[string]string{"PW": "${X:-y}"}}

	if err := svc.Validate("sandbox"); err != nil {
		t.Errorf("Validate refused a literal API env value: %v", err)
	}
}

// `$${` is the way to write a literal `${` (compose's spelling): a password or a template string
// containing `${HOME}` has to be expressible, and it must reach the container as written rather
// than be expanded or refused.
func TestEnvDollarDollarBraceIsALiteral(t *testing.T) {
	t.Setenv("HOME", "/should/not/appear")
	t.Setenv("X", "x")

	for in, want := range map[string]string{
		`$${HOME}`:          `${HOME}`,
		`$${X:-y}`:          `${X:-y}`,
		`pre-$${HOME}-post`: `pre-${HOME}-post`,
		`$${HOME}${X}`:      `${HOME}x`,
		`$${`:               `${`,
		`a$$b`:              `a$$b`,
		`$$${X}`:            `$${X}`,
	} {
		dir := t.TempDir()
		path := dir + "/sandbox.json"
		body := envSpec(strconvQuote(in))

		if err := writeFile(path, body); err != nil {
			t.Fatal(err)
		}

		sp, err := LoadSpec(path)
		if err != nil {
			t.Errorf("env value %q was refused: %v", in, err)
			continue
		}

		if got := sp.Services["a"].Env["PW"]; got != want {
			t.Errorf("env value %q reached the container as %q, want %q", in, got, want)
		}
	}
}

// Every bad form in the file at once, like unset variables: one per run is one failed
// validate per mistake.
func TestEnvSyntaxReportsEveryBadForm(t *testing.T) {
	body := []byte(`{"version":1,"services":{
		"a":{"image":"alpine","ports":[80],"env":{"P":"${X:-y}","Q":"${Y:?z} and ${Z-w}"}},
		"b":{"image":"alpine","ports":[81],"env":{"R":"${}"}}}}`)

	_, err := ParseSpec(body, "spec.json")
	if err == nil {
		t.Fatal("bad forms were accepted")
	}

	for _, want := range []string{`"${X:-y}"`, `"${Y:?z}"`, `"${Z-w}"`, `"${}"`, "a.P", "a.Q", "b.R", "$${"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func writeFile(path string, body []byte) error { return os.WriteFile(path, body, 0o600) }
