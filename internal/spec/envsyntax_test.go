package spec

import (
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
