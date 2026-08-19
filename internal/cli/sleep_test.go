package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// sleeper records which refs were stopped, so a test can assert Sleep parks exactly the running
// services and leaves the asleep ones alone.
type sleeper struct {
	provider.Provider
	units   []provider.Unit
	stopped []string
}

func (s *sleeper) List(context.Context, string) ([]provider.Unit, error) { return s.units, nil }
func (s *sleeper) Stop(_ context.Context, ref string) error {
	s.stopped = append(s.stopped, ref)
	return nil
}

// Sleep stops every RUNNING service and skips the ones already asleep - the pair to Ready, and
// an explicit override of the idle timer rather than a second lifecycle owner.
func TestSleepStopsRunningServicesOnly(t *testing.T) {
	p := &sleeper{units: []provider.Unit{
		{Sandbox: "agent-42", Service: "postgres", Ref: "sbx-agent-42-postgres", Running: true},
		{Sandbox: "agent-42", Service: "redis", Ref: "sbx-agent-42-redis", Running: true},
		{Sandbox: "agent-42", Service: "browser", Ref: "sbx-agent-42-browser", Running: false},
	}}

	if err := Sleep(context.Background(), p, "agent-42"); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(p.stopped, ",")
	want := "sbx-agent-42-postgres,sbx-agent-42-redis"
	if got != want {
		t.Errorf("Sleep stopped %q, want the two running services %q (the asleep one must be left alone)", got, want)
	}
}

// A sandbox that is already fully asleep stops nothing and does not error.
func TestSleepOnAnAlreadyAsleepSandboxIsANoOp(t *testing.T) {
	p := &sleeper{units: []provider.Unit{
		{Sandbox: "x", Service: "db", Ref: "sbx-x-db", Running: false},
	}}

	if err := Sleep(context.Background(), p, "x"); err != nil {
		t.Fatal(err)
	}

	if len(p.stopped) != 0 {
		t.Errorf("Sleep stopped %v on an already-asleep sandbox", p.stopped)
	}
}
