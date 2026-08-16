package ui

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/tui"
)

// fakeProvider records what the dashboard asked it to do.
type fakeProvider struct {
	provider.Provider

	mu      sync.Mutex
	stopped []string
	removed []string
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) List(context.Context, string) ([]provider.Unit, error) {
	return nil, nil
}

func (f *fakeProvider) Stop(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stopped = append(f.stopped, ref)

	return nil
}

func (f *fakeProvider) Remove(_ context.Context, sandbox string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.removed = append(f.removed, sandbox)

	return nil
}

func (f *fakeProvider) Logs(_ context.Context, _ string, _ int, _ bool, w io.Writer) error {
	_, _ = io.WriteString(w, "line one\nline two\n")

	return nil
}

func (f *fakeProvider) took() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.stopped...), append([]string(nil), f.removed...)
}

func newDash(p provider.Provider) *dash {
	return &dash{
		opt:        Options{Provider: p, Version: "v0.1.0"},
		prev:       map[string]provider.Usage{},
		limitsSeen: map[string]time.Time{},
		model: model{
			version: "v0.1.0",
			rows: []row{
				{Sandbox: "a", Service: "postgres", Awake: true, Ref: "sbx-a-postgres", Address: "127.0.0.1:20000"},
				{Sandbox: "b", Service: "redis", Awake: false, Ref: "sbx-b-redis", Address: "127.0.0.1:20001"},
			},
		},
	}
}

func TestMovingTheSelection(t *testing.T) {
	d := newDash(&fakeProvider{})

	d.handle(context.Background(), tui.Key{Code: tui.KeyDown})

	if d.model.selected != 1 {
		t.Errorf("down moved the selection to %d, want 1", d.model.selected)
	}

	// And it stops at the ends rather than running off them.
	d.handle(context.Background(), tui.Key{Code: tui.KeyDown})

	if d.model.selected != 1 {
		t.Errorf("down past the last row moved to %d", d.model.selected)
	}

	d.handle(context.Background(), tui.Key{Code: tui.KeyUp})
	d.handle(context.Background(), tui.Key{Code: tui.KeyUp})

	if d.model.selected != 0 {
		t.Errorf("up past the first row moved to %d", d.model.selected)
	}
}

// Removing a sandbox asks first, and a stray keypress must not be the answer.
func TestRemovingAsksFirst(t *testing.T) {
	p := &fakeProvider{}
	d := newDash(p)

	d.handle(context.Background(), tui.Key{Rune: 'd', Code: tui.KeyRune})

	if d.model.confirm == "" {
		t.Fatal("d removed without asking")
	}

	_, removed := p.took()
	if len(removed) != 0 {
		t.Fatalf("it removed %v before the question was answered", removed)
	}

	// Anything that is not y is no.
	d.handle(context.Background(), tui.Key{Rune: 'x', Code: tui.KeyRune})

	if d.model.confirm != "" {
		t.Error("the question is still pending after an answer")
	}

	time.Sleep(50 * time.Millisecond)

	if _, removed := p.took(); len(removed) != 0 {
		t.Errorf("a keypress that was not y removed %v", removed)
	}
}

func TestConfirmingARemoval(t *testing.T) {
	p := &fakeProvider{}
	d := newDash(p)

	d.handle(context.Background(), tui.Key{Rune: 'd', Code: tui.KeyRune})
	d.handle(context.Background(), tui.Key{Rune: 'y', Code: tui.KeyRune})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, removed := p.took(); len(removed) == 1 && removed[0] == "a" {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	_, removed := p.took()
	t.Errorf("y did not remove the selected sandbox; removed=%v", removed)
}

func TestSleepingAService(t *testing.T) {
	p := &fakeProvider{}
	d := newDash(p)

	d.handle(context.Background(), tui.Key{Rune: 's', Code: tui.KeyRune})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stopped, _ := p.took(); len(stopped) == 1 && stopped[0] == "sbx-a-postgres" {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	stopped, _ := p.took()
	t.Errorf("s did not stop the selected service; stopped=%v", stopped)
}

// Enter wakes the selected service by connecting to it, because connecting is the only way
// anything wakes here. This is the one that shipped broken.
func TestEnterWakesBySelectingAndDialling(t *testing.T) {
	p := &fakeProvider{}
	d := newDash(p)

	// Select the asleep one.
	d.handle(context.Background(), tui.Key{Code: tui.KeyDown})

	if r, _ := d.model.currentRow(); r.Service != "redis" {
		t.Fatalf("the selected row is %q, expected the asleep redis", r.Service)
	}

	d.handle(context.Background(), tui.Key{Code: tui.KeyEnter})

	// Nothing is listening on that address in a test, so what is asserted is that it tried
	// and reported the failure - which is proof the key reached the wake path at all.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		msg := d.model.message
		d.mu.Unlock()

		if msg != "" {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Error("Enter produced no message at all, so it never reached the wake path")
}

func TestQuitting(t *testing.T) {
	d := newDash(&fakeProvider{})

	if !d.handle(context.Background(), tui.Key{Rune: 'q', Code: tui.KeyRune}) {
		t.Error("q did not quit")
	}

	if !d.handle(context.Background(), tui.Key{Code: tui.KeyCtrlC}) {
		t.Error("ctrl-c did not quit")
	}
}

// Logs are a pane, not an overlay: pressing l switches what the bottom shows and moves
// nothing else. Pressing it again switches back.
func TestLPaneTogglesWithoutChangingTheLayout(t *testing.T) {
	d := newDash(&fakeProvider{})

	if d.model.pane != paneEvents {
		t.Fatal("the pane does not start on events")
	}

	d.handle(context.Background(), tui.Key{Rune: 'l', Code: tui.KeyRune})

	if d.model.pane != paneLogs {
		t.Error("l did not switch the pane to logs")
	}

	d.handle(context.Background(), tui.Key{Rune: 'l', Code: tui.KeyRune})

	if d.model.pane != paneEvents {
		t.Error("l a second time did not switch back to events")
	}
}
