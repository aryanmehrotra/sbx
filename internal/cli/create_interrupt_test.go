package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/slotlock"
)

// A plain `sbx create` had no signal handling: SIGTERM during a health wait that never passes
// killed it with rc 143, no word about the half-built sandbox, and its name lock left behind.
// Interrupted, it stops, says what it left and how to finish or remove it, lets go of its locks,
// and hands back the signal - which is what main turns into 130 or 143, and it does that only
// for an error that is the signal itself, not one wrapping it.
func TestAnInterruptedCreateSaysWhatItLeftAndReleasesItsLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.probe = func(string) (bool, bool) { return false, true } // a health check that never passes

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	time.AfterFunc(300*time.Millisecond, func() { cancel(&Interrupted{Signal: syscall.SIGTERM}) })

	var err error

	began := time.Now()
	out := captureOutput(t, func() {
		err = Create(ctx, p, redisSpec(t), "tm", false, provider.IsolationContainer, nil)
	})

	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("the interrupt did not end the create: it took %s", took)
	}

	c, ok := err.(interface{ ChildStatus() int })
	if !ok || c.ChildStatus() != 143 {
		t.Errorf("want the signal itself back, exiting 143; got %T: %v", err, err)
	}

	for _, want := range []string{"interrupted", "sbx rm tm", "sbx create tm", "redis"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}

	if !p.has("tm") {
		t.Error("what the create placed was removed; a failed create keeps it, and so does an interrupted one")
	}

	lock, _ := slotlock.NamePath("tm")
	if _, err := os.Stat(lock); err == nil {
		t.Errorf("the interrupted create left its name lock %s", lock)
	}
}

// Interrupted before anything existed, it says so rather than pointing at a sandbox to remove.
func TestACreateInterruptedBeforeAnythingExistedSaysSo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&Interrupted{Signal: syscall.SIGINT})

	var err error

	out := captureOutput(t, func() {
		err = Create(ctx, newRaceStub(), redisSpec(t), "early", false, provider.IsolationContainer, nil)
	})

	var in *Interrupted
	if !errors.As(err, &in) {
		t.Fatalf("want the interrupt back, got %v", err)
	}

	if c, ok := err.(interface{ ChildStatus() int }); !ok || c.ChildStatus() != 130 {
		t.Errorf("want exit 130 for SIGINT, got %T: %v", err, err)
	}

	if strings.Contains(out, "sbx rm") || !strings.Contains(strings.ToLower(out), "nothing was created") {
		t.Errorf("want it to say nothing was created, not advise removing:\n%s", out)
	}
}
