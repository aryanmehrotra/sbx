//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/slotlock"
)

// The signal is turned into a cancelled context whose cause names it, which is what With and
// the command both read. A real signal to this process, so the handler is what is tested.
func TestSignalContextCarriesTheSignal(t *testing.T) {
	ctx, stop := SignalContext(context.Background())
	defer stop()

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Skip(err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not cancel the context")
	}

	var in *Interrupted
	if !errors.As(context.Cause(ctx), &in) || in.ChildStatus() != 130 {
		t.Errorf("want an Interrupted cause with status 130, got %v", context.Cause(ctx))
	}
}

// A real SIGTERM to a create under CreateSignalContext - the handler `sbx create` installs -
// ends the create with the signal's own status and no name lock left behind. Without a
// handler the signal killed the process, which is also this test's.
func TestSIGTERMStopsACreateAndReleasesItsLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.probe = func(string) (bool, bool) { return false, true }

	ctx, stop := CreateSignalContext(context.Background())
	defer stop()

	time.AfterFunc(300*time.Millisecond, func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) })

	var err error

	_ = captureOutput(t, func() {
		err = Create(ctx, p, redisSpec(t), "sig", false, provider.IsolationContainer, nil)
	})

	c, ok := err.(interface{ ChildStatus() int })
	if !ok || c.ChildStatus() != 143 {
		t.Errorf("want exit 143, got %T: %v", err, err)
	}

	lock, _ := slotlock.NamePath("sig")
	if _, serr := os.Stat(lock); serr == nil {
		t.Errorf("the name lock %s was left behind", lock)
	}
}
