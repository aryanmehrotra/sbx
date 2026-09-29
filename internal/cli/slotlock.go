package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/aryanmehrotra/sbx/internal/slotlock"
)

// The slot lock lives in its own package because two things now create sandboxes - this
// package and the OpenSandbox API inside the daemon - and a lock only one of them takes does
// not serialise anything. See internal/slotlock for why it exists at all.

// lockSlots waits for the slot lock. A wait that runs out is an error naming the holder: going
// ahead without the lock is the race it exists to prevent.
func lockSlots(ctx context.Context) (func(), error) {
	return slotlock.Acquire(ctx, func(pid int) {
		fmt.Fprintf(os.Stderr, "  waiting for pid %d (another sbx create) to claim its slot...\n", pid)
	})
}

// lockName waits for sandbox's name lock, which a create or add holds from reading what the
// sandbox has to the end of making what it lacks.
func lockName(ctx context.Context, sandbox string) (func(), error) {
	return slotlock.AcquireName(ctx, sandbox, func(pid int) {
		fmt.Fprintf(os.Stderr, "  waiting for pid %d, which is creating or changing sandbox %q...\n", pid, sandbox)
	})
}

func slotLockPath() (string, error) { return slotlock.Path() }
