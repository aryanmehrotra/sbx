package cli

import "github.com/aryanmehrotra/sbx/internal/slotlock"

// The slot lock lives in its own package because two things now create sandboxes - this
// package and the OpenSandbox API inside the daemon - and a lock only one of them takes does
// not serialise anything. See internal/slotlock for why it exists at all.

func lockSlots() func() { return slotlock.Lock() }

func slotLockPath() (string, error) { return slotlock.Path() }
