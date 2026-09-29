package cli

// Two derived names that land on the same variable, and a derived name an export already holds.
// Both used to resolve silently: the export or the service sorted first kept the name, and the
// other service simply had no variables - found only by a client dialling the wrong service.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

func captureEnvWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	old := envWarnings
	envWarnings = &buf

	t.Cleanup(func() { envWarnings = old })

	return &buf
}

const exportsYAtX = `{
  "version": 1,
  "services": {"x": {"image": "redis:7-alpine", "ports": [6379]}},
  "exports": {"Y_PORT": "x:6379"}
}`

// The export wins - it is the spec author's contract - and y is named as having lost it.
func TestEnvWarnsWhenAnExportHoldsAServicesDerivedName(t *testing.T) {
	warned := captureEnvWarnings(t)

	p := &envFake{units: []provider.Unit{
		{Sandbox: "sb", Service: "x", Ref: "r1", Slot: 1, Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20010}}},
		{Sandbox: "sb", Service: "y", Ref: "r2", Slot: 1, Index: 1, Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20011}}},
	}}

	vars, err := envVars(context.Background(), p, writeSpec(t, exportsYAtX), "sb")
	if err != nil {
		t.Fatal(err)
	}

	if got := varsOf(vars)["Y_PORT"]; got != "20010" {
		t.Errorf("Y_PORT = %q, want the export's 20010 (x)", got)
	}

	for _, want := range []string{`"y"`, "Y_PORT", "export", "exports"} {
		if !strings.Contains(warned.String(), want) {
			t.Errorf("the warning does not say %q:\n%s", want, warned.String())
		}
	}
}

// Neither gets the name: whichever sorted first used to take it, so MY_CACHE_PORT pointed at one
// of two services depending on spelling, with nothing to say which.
func TestEnvGivesACollidingNameToNeitherService(t *testing.T) {
	warned := captureEnvWarnings(t)

	p := &envFake{units: []provider.Unit{
		{Sandbox: "sb", Service: "x", Ref: "r1", Slot: 1, Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20010}}},
		{Sandbox: "sb", Service: "my.cache", Ref: "r2", Slot: 1, Index: 1, Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20011}}},
		{Sandbox: "sb", Service: "my-cache", Ref: "r3", Slot: 1, Index: 2, Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20012}}},
	}}

	vars, err := envVars(context.Background(), p, writeSpec(t, exportsYAtX), "sb")
	if err != nil {
		t.Fatal(err)
	}

	m := varsOf(vars)
	for _, k := range []string{"MY_CACHE_PORT", "MY_CACHE_HOST"} {
		if v, ok := m[k]; ok {
			t.Errorf("%s = %q was handed to one of two services that both derive it", k, v)
		}
	}

	for _, want := range []string{`"my-cache"`, `"my.cache"`, "MY_CACHE_PORT", "exports"} {
		if !strings.Contains(warned.String(), want) {
			t.Errorf("the warning does not say %q:\n%s", want, warned.String())
		}
	}
}

// No collision, no warning: stderr noise on every `eval "$(sbx env)"` would be ignored in a week.
func TestEnvIsQuietWithoutACollision(t *testing.T) {
	warned := captureEnvWarnings(t)
	t.Setenv("SBX_FX3_TEST_UNSET_SECRET", "x")

	p := &envFake{units: []provider.Unit{
		{Sandbox: "sb", Service: "postgres", Ref: "r1", Slot: 1, Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20010}}},
		{Sandbox: "sb", Service: "cache", Ref: "r2", Slot: 1, Index: 1, Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20011}}},
	}}

	if _, err := envVars(context.Background(), p, writeSpec(t, secretSpec), "sb"); err != nil {
		t.Fatal(err)
	}

	if warned.Len() != 0 {
		t.Errorf("warned with nothing colliding:\n%s", warned.String())
	}
}
