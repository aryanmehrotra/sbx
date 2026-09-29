//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
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
