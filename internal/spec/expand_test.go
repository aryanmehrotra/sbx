package spec

import (
	"strings"
	"testing"
)

func fixed(vals map[string]string) func(string) (string, bool) {
	return func(n string) (string, bool) {
		v, ok := vals[n]
		return v, ok
	}
}

func specWith(env map[string]string) *Spec {
	return &Spec{
		Version:  1,
		Services: map[string]Service{"db": {Image: "postgres", Ports: []int{5432}, Env: env}},
	}
}

func TestExpandsReferencedVariables(t *testing.T) {
	s := specWith(map[string]string{
		"POSTGRES_PASSWORD": "${DB_PASSWORD}",
		"POSTGRES_USER":     "app",
		"MIXED":             "prefix-${DB_PASSWORD}-suffix",
	})

	if err := s.expandEnv(fixed(map[string]string{"DB_PASSWORD": "s3cret"})); err != nil {
		t.Fatalf("expandEnv: %v", err)
	}

	got := s.Services["db"].Env

	if got["POSTGRES_PASSWORD"] != "s3cret" {
		t.Errorf("POSTGRES_PASSWORD = %q, want s3cret", got["POSTGRES_PASSWORD"])
	}

	if got["POSTGRES_USER"] != "app" {
		t.Errorf("a literal value was altered: %q", got["POSTGRES_USER"])
	}

	if got["MIXED"] != "prefix-s3cret-suffix" {
		t.Errorf("MIXED = %q", got["MIXED"])
	}
}

// The failure that must not be silent. A database that comes up with an empty password
// because a variable was not exported looks like success, and finding out later is
// expensive - so this refuses at load, before anything is created.
func TestAnUnsetVariableIsAnError(t *testing.T) {
	s := specWith(map[string]string{"POSTGRES_PASSWORD": "${NOT_SET_ANYWHERE}"})

	err := s.expandEnv(fixed(nil))
	if err == nil {
		t.Fatal("an unset variable was accepted - the service would start with the literal " +
			"${NOT_SET_ANYWHERE} or an empty string")
	}

	for _, want := range []string{"NOT_SET_ANYWHERE", "db.POSTGRES_PASSWORD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// All of them at once: one failed create per missing variable is one round trip per
// variable to find out what the environment needs.
func TestEveryMissingVariableIsReportedTogether(t *testing.T) {
	s := specWith(map[string]string{"A": "${ONE}", "B": "${TWO}", "C": "${THREE}"})

	err := s.expandEnv(fixed(nil))
	if err == nil {
		t.Fatal("accepted three unset variables")
	}

	for _, want := range []string{"ONE", "TWO", "THREE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q is missing from the error: %v", want, err)
		}
	}
}

// A bare $NAME is not a reference. Braces make the boundary unambiguous, and a password
// that legitimately contains a dollar sign must survive unchanged.
func TestABareDollarIsNotASubstitution(t *testing.T) {
	s := specWith(map[string]string{"PASSWORD": "p$$w0rd$HOME"})

	if err := s.expandEnv(fixed(map[string]string{"HOME": "/root"})); err != nil {
		t.Fatalf("expandEnv: %v", err)
	}

	if got := s.Services["db"].Env["PASSWORD"]; got != "p$$w0rd$HOME" {
		t.Errorf("a literal dollar was substituted: %q", got)
	}
}

// Empty is a real value, distinct from unset. Someone who exports FOO= meant empty.
func TestAnEmptyValueIsNotMissing(t *testing.T) {
	s := specWith(map[string]string{"OPTIONAL": "${MAYBE}"})

	if err := s.expandEnv(fixed(map[string]string{"MAYBE": ""})); err != nil {
		t.Fatalf("an explicitly empty variable was treated as unset: %v", err)
	}
}
