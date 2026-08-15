package cli

// The negotiated capabilities, and the one command that must not wake anything.
//
// DECISIONS.md: "a method on the core interface is a promise that every provider keeps", so
// snapshot, build, prewarm and collect are optional interfaces a provider either implements
// or does not. The rule that makes that honest is that a provider which cannot do the thing
// says so, naming itself — not a stub returning nil, and not a silent no-op.
//
// Three of the four refusals had no test. A regression turning any of them into a silent
// success would have shipped: `sbx snapshot` against a cluster would print that it saved
// something and save nothing.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// bare implements Provider and nothing else — no Snapshotter, no Builder, no Collector, no
// Puller. This is what the kubernetes provider looks like to each of those type assertions.
type bare struct {
	provider.Provider

	started []string
	units   []provider.Unit
}

func (b *bare) Name() string { return "kubernetes" }

func (b *bare) Start(_ context.Context, ref string) error {
	b.started = append(b.started, ref)

	return nil
}

func (b *bare) List(context.Context, string) ([]provider.Unit, error) { return b.units, nil }

func (b *bare) Logs(_ context.Context, _ string, _ int, _ bool, w io.Writer) error {
	_, err := io.WriteString(w, "a log line\n")

	return err
}

// Each refusal must name the backend. "not supported" sends someone to the wrong file; "the
// kubernetes provider cannot …" tells them the answer is to use a different provider.
func TestCapabilityRefusalsNameTheBackend(t *testing.T) {
	p := &bare{}

	cases := map[string]func() error{
		"snapshot": func() error { _, err := provider.SnapshotterFor(p); return err },
		"build":    func() error { _, err := provider.BuilderFor(p); return err },
		"collect":  func() error { _, err := provider.CollectorFor(p); return err },
		"prewarm":  func() error { _, err := provider.PullerFor(p); return err },
	}

	for name, call := range cases {
		err := call()
		if err == nil {
			t.Errorf("%s: a provider that cannot do this was accepted — the command would "+
				"report success and do nothing", name)

			continue
		}

		if !strings.Contains(err.Error(), "kubernetes") {
			t.Errorf("%s: refusal does not name the backend: %v", name, err)
		}
	}
}

// The commands that need a capability must refuse through it rather than panicking or
// half-running.
func TestCommandsRefuseWhenTheProviderCannot(t *testing.T) {
	p := &bare{}

	if _, err := Snapshot(context.Background(), p, "sandbox", "name"); err == nil {
		t.Error("snapshot succeeded against a provider that cannot snapshot")
	}

	var out bytes.Buffer
	if err := GC(context.Background(), p, &out, time.Hour, false, false); err == nil {
		t.Error("gc succeeded against a provider that cannot collect")
	}

	if err := Prewarm(context.Background(), p, &out, []string{"nginx"}); err == nil {
		t.Error("prewarm succeeded against a provider that cannot pull")
	}
}

// README: "Everything wakes what it touches — except `logs`, because asking what a sandbox
// said isn't using it."
//
// It was not true. `sbx logs <sandbox> <service>` resolved the container through the same
// helper exec uses, which starts a stopped container first. A `sbx logs -f` left open would
// hold a sandbox awake for as long as anybody was watching it, which is the exact opposite
// of what this command should do.
func TestLogsDoesNotWakeTheSandbox(t *testing.T) {
	p := &bare{units: []provider.Unit{{Service: "postgres", Ref: "sbx-b-postgres", Running: false}}}

	if err := Logs(context.Background(), p, "b", "postgres", 5, false); err != nil {
		t.Fatalf("Logs: %v", err)
	}

	if len(p.started) > 0 {
		t.Errorf("logs started %v — reading what a sandbox said is not using it, and "+
			"`sbx logs -f` would hold it awake for as long as somebody watched", p.started)
	}
}

// And it must still say something useful when the service is not there, rather than an empty
// success or a reference to a container name the user never typed.
func TestLogsNamesTheServicesItHas(t *testing.T) {
	p := &bare{units: []provider.Unit{{Service: "postgres", Ref: "r"}, {Service: "redis", Ref: "r2"}}}

	err := Logs(context.Background(), p, "b", "nope", 5, false)
	if err == nil {
		t.Fatal("logs for a service that does not exist reported success")
	}

	for _, want := range []string{"nope", "postgres", "redis"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
