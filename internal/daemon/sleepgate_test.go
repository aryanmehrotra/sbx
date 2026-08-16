package daemon

// A sandbox that has never been seen serving must not be slept.
//
// This is the gate from a real incident, recorded in DECISIONS.md: the activator scaled a
// sandbox to zero 39 seconds into its own creation, while the command creating it was still
// waiting for the first health check. Nothing was idle - it had simply never had a byte,
// because it was not up yet.
//
// Every other daemon test starts from served=true to get past this gate and test something
// else, which left the gate itself with no test at all. Deleting it would have kept CI green
// and put the incident straight back.

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// notYetServing is a workload that is running but has not come up: it declares a health
// check, and that check says no. A database replaying WAL on first boot looks exactly like
// this, for as long as it takes.
type notYetServing struct {
	countingProvider
}

func (n *notYetServing) Healthy(context.Context, string) (bool, bool) {
	return false, true // not serving, and it did declare a check - so the answer is real
}

func TestReapDoesNotSleepAUnitThatWasNeverServing(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &notYetServing{}

	// Awake and long past the idle window: every condition for sleep is met except the one
	// that matters.
	u := newUnit("boot", "postgres", "ref", "boot/postgres", nil, true)
	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

	d := &daemon{provider: p, idle: time.Minute, units: map[string]*unit{u.name: u}}

	// Swept repeatedly, with the idle clock forced back to long-ago before each pass. One
	// tick is not enough to catch this: a gate that merely fails to sleep it on the first
	// pass, while wrongly recording that it HAS been seen serving, sleeps it on the next one
	// - which is precisely the 39-seconds-into-creation shape of the original incident.
	for i := range 5 {
		u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

		d.reap(context.Background())

		if got := p.stops.Load(); got != 0 {
			t.Fatalf("sweep %d stopped a sandbox that had never served (%d times) - this is "+
				"the incident that put a creating sandbox to sleep 39 seconds in", i+1, got)
		}
	}

	if !u.isAwake() {
		t.Error("the unit was marked asleep without being stopped, which is worse: the daemon " +
			"and the container now disagree about what is running")
	}
}

// And the gate must open once the workload does come up, or nothing ever sleeps and the
// whole product is a container that stays running.
func TestReapSleepsOnceItHasBeenSeenServing(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingProvider{}
	p.serving.Store(true)

	u := newUnit("up", "redis", "ref", "up/redis", nil, true)

	d := &daemon{provider: p, idle: time.Minute, units: map[string]*unit{u.name: u}}

	// First pass: healthy, so sleepable() records that it has been seen serving - and
	// restarts the idle clock, because being seen serving is the first moment idleness can
	// mean anything. So this pass must NOT sleep it.
	d.reap(context.Background())

	if got := p.stops.Load(); got != 0 {
		t.Fatalf("slept on the very first healthy observation (%d) - the idle clock had not "+
			"yet had a chance to run from a meaningful start", got)
	}

	// Now let it be genuinely idle.
	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

	d.reap(context.Background())

	if got := p.stops.Load(); got != 1 {
		t.Fatalf("an idle, previously-serving sandbox was stopped %d times, want 1", got)
	}

	if u.isAwake() {
		t.Error("still marked awake after being stopped")
	}
}

// A unit the daemon already believes is asleep is not stopped again on every tick.
func TestReapIgnoresSleepingUnits(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingProvider{}

	u := newUnit("down", "redis", "ref", "down/redis", nil, false)
	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

	d := &daemon{provider: p, idle: time.Minute, units: map[string]*unit{u.name: u}}

	d.reap(context.Background())

	if got := p.stops.Load(); got != 0 {
		t.Errorf("stopped an already-sleeping unit %d times", got)
	}
}

var _ provider.Provider = (*notYetServing)(nil)
