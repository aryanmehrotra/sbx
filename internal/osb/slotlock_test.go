package osb

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/slotlock"
)

// holdSlotLock makes the machine's slot lock look held by another live process (this test's
// parent), with a wait short enough for a test, and the real lock in place of the harness's
// no-op.
func holdSlotLock(t *testing.T) (option, int) {
	t.Helper()

	t.Setenv("HOME", t.TempDir())

	path, err := slotlock.Path()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	holder := os.Getppid()
	if err := os.WriteFile(path, []byte(fmt.Sprint(holder)), 0o644); err != nil {
		t.Fatal(err)
	}

	old := slotlock.SlotWait
	t.Cleanup(func() { slotlock.SlotWait = old })
	slotlock.SlotWait = 200 * time.Millisecond

	return func(_ *harness, o *Options) { o.LockSlots = nil }, holder
}

// An API create whose wait for the slot lock runs out used to go ahead without it - the race
// the lock exists to prevent, measured on the CLI as two sandboxes on one slot. It now fails
// with a reason of its own, naming the holder, and places no container.
func TestAnAPICreateFailsWhenTheSlotLockWaitRunsOut(t *testing.T) {
	hold, holder := holdSlotLock(t)
	h := newHarness(t, hold)

	got := h.create(minimalCreate())

	assertCarried(t, h, got, "slot_lock_timeout", fmt.Sprint(holder))

	h.p.mu.Lock()
	defer h.p.mu.Unlock()

	if len(h.p.created) != 0 {
		t.Errorf("a container was placed without the slot lock: %v", h.p.created)
	}
}

// The same on the docker provider's branch, which picks its own slot and holds the lock only
// for the choice - and must not leave the slot it never picked reserved.
func TestAPickedSlotCreateFailsWhenTheSlotLockWaitRunsOut(t *testing.T) {
	hold, holder := holdSlotLock(t)
	pd := &pickingDocker{fakeDocker: newFakeDocker()}
	h := newHarness(t, hold, func(_ *harness, o *Options) { o.Provider = pd })

	got := h.create(minimalCreate())

	assertCarried(t, h, got, "slot_lock_timeout", fmt.Sprint(holder))

	pd.mu.Lock()
	placed := len(pd.created)
	pd.mu.Unlock()

	if placed != 0 {
		t.Errorf("a container was placed without the slot lock: %d", placed)
	}
}
