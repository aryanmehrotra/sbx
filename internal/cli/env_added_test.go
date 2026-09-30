package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// envFake addresses services the way the docker provider does: a loopback port per ordinal.
type envFake struct {
	provider.Provider
	units []provider.Unit
}

func (f *envFake) Name() string { return "fake" }

func (f *envFake) List(context.Context, string) ([]provider.Unit, error) { return f.units, nil }

func (f *envFake) Endpoints(_, _ string, slot, start int, ports []int) []provider.Endpoint {
	eps := make([]provider.Endpoint, 0, len(ports))
	for i := range ports {
		eps = append(eps, provider.Endpoint{Host: "127.0.0.1", Port: 20000 + slot*10 + start + i})
	}

	return eps
}

// A spec whose one service reads a secret from the environment, and exports its port.
const secretSpec = `{
  "version": 1,
  "services": {
    "postgres": {
      "image": "postgres:16-alpine",
      "ports": [5432],
      "env": {"POSTGRES_PASSWORD": "${SBX_FX3_TEST_UNSET_SECRET}"}
    }
  },
  "exports": {"DATABASE_PORT": "postgres:5432"}
}`

func writeSpec(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func varsOf(vars [][2]string) map[string]string {
	m := map[string]string{}
	for _, kv := range vars {
		m[kv[0]] = kv[1]
	}

	return m
}

// `sbx env` prints ports. It needs no secret to do that, so a ${VAR} only a container's env
// reads must not make it fail - the shell asking for ports is often not the one holding the
// secret (a second terminal, a CI step after create).
func TestEnvDoesNotNeedTheSecretsOnlyAContainerReads(t *testing.T) {
	_ = os.Unsetenv("SBX_FX3_TEST_UNSET_SECRET")

	p := &envFake{units: []provider.Unit{{Sandbox: "sb", Service: "postgres", Ref: "r", Slot: 1,
		Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20010}}}}}

	vars, err := envVars(context.Background(), p, writeSpec(t, secretSpec), "sb")
	if err != nil {
		t.Fatalf("sbx env failed on an unset secret it never uses: %v", err)
	}

	if got := varsOf(vars)["DATABASE_PORT"]; got != "20010" {
		t.Errorf("DATABASE_PORT = %q, want 20010", got)
	}
}

// A service added with `sbx add` is not in sandbox.json, so it has no export. `sbx env` still
// has to say where it is, or the only way to find the port is `sbx list` and a copy-paste.
func TestEnvIncludesServicesAddedOutsideTheSpec(t *testing.T) {
	t.Setenv("SBX_FX3_TEST_UNSET_SECRET", "x")

	p := &envFake{units: []provider.Unit{
		{Sandbox: "sb", Service: "postgres", Ref: "r1", Slot: 1, Running: true,
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20010}}},
		// Asleep: it wakes on connect, so it is as addressable as a running one.
		{Sandbox: "sb", Service: "my-cache.v2", Ref: "r2", Slot: 1, Index: 3,
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20013}, {Host: "127.0.0.1", Port: 20014}}},
		// No ports: nothing to export.
		{Sandbox: "sb", Service: "worker", Ref: "r3", Slot: 1, Running: true},
	}}

	vars, err := envVars(context.Background(), p, writeSpec(t, secretSpec), "sb")
	if err != nil {
		t.Fatal(err)
	}

	m := varsOf(vars)

	if m["MY_CACHE_V2_PORT"] != "20013" || m["MY_CACHE_V2_HOST"] != "127.0.0.1" {
		t.Errorf("added service missing or wrong: MY_CACHE_V2_PORT=%q MY_CACHE_V2_HOST=%q (all: %v)",
			m["MY_CACHE_V2_PORT"], m["MY_CACHE_V2_HOST"], m)
	}

	// The exported service keeps exactly its export - no second POSTGRES_PORT beside it.
	if _, dup := m["POSTGRES_PORT"]; dup {
		t.Errorf("a service already covered by an export got a derived variable too: %v", m)
	}

	if _, ok := m["WORKER_PORT"]; ok {
		t.Errorf("a service with no ports got a port variable: %v", m)
	}
}

// A derived name never overrides an export: the spec author's name is the contract.
func TestEnvDerivedNameNeverOverridesAnExport(t *testing.T) {
	t.Setenv("SBX_FX3_TEST_UNSET_SECRET", "x")

	p := &envFake{units: []provider.Unit{
		{Sandbox: "sb", Service: "postgres", Ref: "r1", Slot: 1,
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20010}}},
		{Sandbox: "sb", Service: "database", Ref: "r2", Slot: 1, Index: 1,
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20011}}},
	}}

	vars, err := envVars(context.Background(), p, writeSpec(t, secretSpec), "sb")
	if err != nil {
		t.Fatal(err)
	}

	count := 0

	for _, kv := range vars {
		if kv[0] == "DATABASE_PORT" {
			count++

			if kv[1] != "20010" {
				t.Errorf("DATABASE_PORT = %s, want the export's 20010", kv[1])
			}
		}
	}

	if count != 1 {
		t.Errorf("DATABASE_PORT emitted %d times, want once: %v", count, vars)
	}
}

// `sbx add` keeps clear of the ordinals the spec reserves for its optional services. It read
// those through the expanding loader and ignored its error, so one unset ${VAR} silently threw
// the reservations away and the added service took the slot the optional one needs.
func TestAddKeepsSpecReservationsWithAnUnsetSecret(t *testing.T) {
	_ = os.Unsetenv("SBX_FX3_TEST_UNSET_SECRET")

	path := writeSpec(t, `{
  "version": 1,
  "services": {
    "postgres": {"image": "postgres:16-alpine", "ports": [5432],
                 "env": {"POSTGRES_PASSWORD": "${SBX_FX3_TEST_UNSET_SECRET}"}},
    "zz-optional": {"image": "clickhouse:24", "ports": [8123], "optional": true}
  }
}`)

	units := []provider.Unit{{Sandbox: "sb", Service: "postgres", Index: 0,
		Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20010}}}}

	got, err := freeIndex(path, units, 1)
	if err != nil {
		t.Fatal(err)
	}

	if got < 2 {
		t.Errorf("freeIndex = %d, which the optional service reserves; want 2 or later", got)
	}
}
