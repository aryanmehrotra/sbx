package daemon

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/snapshotpause"
)

// `sbx snapshot` pauses running services for its copy. A discovery tick that lands inside that
// pause saw a paused container, called it frozen and not awake ("was stopped outside sbx"), and
// after the snapshot thawed it the daemon believed a running container asleep - one it would
// never sleep again. A pause the snapshot has marked is not the daemon's to correct.
func TestASnapshotPauseIsNotCorrectedAsAFreeze(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Setenv("HOME", t.TempDir())

	for _, held := range []bool{true, false} {
		u := newUnit("t", "db", "sbx-t-db", "inst", "sbx-t-db", nil, true)
		u.setAwake(true)

		// correctAwake asks the provider again before it trusts a listing that contradicts it
		// (r2/daemon-idle), so the provider must say what an engine would: still paused.
		paused := provider.Unit{Sandbox: "t", Service: "db", Ref: u.ref, Paused: true}
		d := &daemon{provider: &listsOneUnit{unit: paused}, units: map[string]*unit{u.ref: u}}

		release := func() {}
		if held {
			release = snapshotpause.Hold(u.ref)
		}

		d.correctAwake(paused)
		release()

		// Unmarked, the existing behaviour stands: a pause outside sbx is a freeze to thaw on
		// the next wake. That half proves the test can tell the two apart.
		if got := u.isAwake(); got != held {
			t.Errorf("held=%v: awake after the tick = %v, want %v", held, got, held)
		}

		if got := u.isFrozen(); got == held {
			t.Errorf("held=%v: frozen after the tick = %v, want %v", held, got, !held)
		}
	}
}

// Nor does the reaper sleep a service mid-snapshot: stopping or freezing it under the copy
// would leave the daemon's belief wrong again once the snapshot thaws it.
func TestTheReaperLeavesASnapshotPauseAlone(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Setenv("HOME", t.TempDir())

	r := &transitionRecorder{}
	u := newUnit("t", "db", "sbx-t-db", "inst", "sbx-t-db", nil, true)
	u.setAwake(true)
	u.served = true // it has been serving, so idleness can sleep it
	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

	d := &daemon{provider: r, units: map[string]*unit{u.ref: u}, idle: time.Millisecond}

	events := func() int {
		r.mu.Lock()
		defer r.mu.Unlock()

		return len(r.events)
	}

	release := snapshotpause.Hold(u.ref)
	d.reap(context.Background())
	release()

	if n := events(); n != 0 || !u.isAwake() {
		t.Errorf("the reaper acted on a unit a snapshot holds paused: %d events, awake=%v", n, u.isAwake())
	}

	// And the control: unmarked, the same unit is slept, so the test can tell.
	d.reap(context.Background())

	if events() == 0 {
		t.Error("the unmarked unit was not slept either; the test proves nothing")
	}
}
