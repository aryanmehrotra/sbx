package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// checkpointer is a Provider that implements Checkpointer and records what it was asked to do,
// so a test can assert the orchestration without a CRIU-capable docker daemon (which no macOS
// host has). The live dump/restore is exercised by a Linux host and by scripts, not here.
type checkpointer struct {
	provider.Provider
	units []provider.Unit
	calls []string
}

func (c *checkpointer) Name() string                                          { return "docker" }
func (c *checkpointer) List(context.Context, string) ([]provider.Unit, error) { return c.units, nil }

func (c *checkpointer) Checkpoint(_ context.Context, ref, name string, leaveRunning bool) error {
	c.calls = append(c.calls, "checkpoint "+ref+" "+name+" leaveRunning="+boolStr(leaveRunning))
	return nil
}

func (c *checkpointer) Restore(_ context.Context, ref, name string) error {
	c.calls = append(c.calls, "restore "+ref+" "+name)
	return nil
}

func (c *checkpointer) Checkpoints(context.Context, string) ([]string, error) { return nil, nil }

func boolStr(b bool) string {
	if b {
		return "true"
	}

	return "false"
}

// Checkpoint dumps exactly the RUNNING services - an asleep one has no process to freeze - and
// takes them frozen (leaveRunning=false), which is what "park a REPL to resume later" means.
func TestCheckpointFreezesRunningServicesOnly(t *testing.T) {
	c := &checkpointer{units: []provider.Unit{
		{Sandbox: "agent-9", Service: "postgres", Ref: "sbx-agent-9-postgres", Running: true},
		{Sandbox: "agent-9", Service: "repl", Ref: "sbx-agent-9-repl", Running: true},
		{Sandbox: "agent-9", Service: "browser", Ref: "sbx-agent-9-browser", Running: false},
	}}

	if err := Checkpoint(context.Background(), c, "agent-9", "mid-thought"); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(c.calls, "\n")
	for _, want := range []string{
		"checkpoint sbx-agent-9-postgres mid-thought leaveRunning=false",
		"checkpoint sbx-agent-9-repl mid-thought leaveRunning=false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	if strings.Contains(got, "browser") {
		t.Errorf("checkpointed the asleep browser, which has no process to freeze:\n%s", got)
	}
}

// Resume restores every service, including the ones a checkpoint froze - a resume is how they
// come back, so skipping the stopped ones would strand exactly the state this exists to keep.
func TestResumeRestoresEveryService(t *testing.T) {
	c := &checkpointer{units: []provider.Unit{
		{Service: "postgres", Ref: "sbx-a-postgres", Running: false},
		{Service: "repl", Ref: "sbx-a-repl", Running: false},
	}}

	if err := Resume(context.Background(), c, "a", "mid-thought"); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(c.calls, "\n")
	for _, want := range []string{"restore sbx-a-postgres mid-thought", "restore sbx-a-repl mid-thought"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// plain is a Provider with no CRIU path at all - the kubernetes shape, whose answer is a
// per-node shim the operator installs. CheckpointerFor must refuse it by name rather than let
// the CLI reach around the backend.
type plain struct {
	provider.Provider
	units []provider.Unit
}

func (plain) Name() string                                            { return "kubernetes" }
func (p plain) List(context.Context, string) ([]provider.Unit, error) { return p.units, nil }

func TestCheckpointRefusedByABackendWithoutCRIU(t *testing.T) {
	err := Checkpoint(context.Background(), plain{}, "x", "snap")
	if err == nil {
		t.Fatal("expected a refusal from a backend that cannot checkpoint memory")
	}

	if !strings.Contains(err.Error(), "kubernetes") || !strings.Contains(err.Error(), "checkpoint memory") {
		t.Errorf("refusal should name the backend and the capability, got: %v", err)
	}
}
