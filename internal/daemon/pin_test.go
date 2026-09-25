package daemon

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

// A pinned sandbox is never idled, however long it goes without a byte: a warm-pool member
// waits for its caller with no traffic at all, and one the reaper froze would cost the claim a
// thaw. Unpinned, the idle clock starts from the unpin - the claim - not from when it was made.
func TestPinnedSandboxIsNotIdledAndUnpinningRestartsItsClock(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingProvider{}
	p.serving.Store(true)

	u := newUnit("osb-aaaaaaaaaaaa", "sandbox", "ref", "inst-ref", "osb-aaaaaaaaaaaa/sandbox", nil, true)
	u.served = true

	d := &daemon{provider: p, idle: time.Minute, units: map[string]*unit{u.name: u}}

	d.Pin("osb-aaaaaaaaaaaa", true)

	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())
	d.reap(context.Background())

	if got := p.stops.Load(); got != 0 {
		t.Fatalf("a pinned sandbox was slept (%d stops)", got)
	}

	d.Pin("osb-aaaaaaaaaaaa", false)
	d.reap(context.Background())

	if got := p.stops.Load(); got != 0 {
		t.Fatalf("slept straight after the unpin (%d): the idle clock must start at the claim", got)
	}

	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())
	d.reap(context.Background())

	if got := p.stops.Load(); got != 1 {
		t.Fatalf("an unpinned, idle sandbox was stopped %d times, want 1", got)
	}
}

// A pin set before the daemon has discovered the unit still applies once it does.
func TestPinCoversUnitsNotYetDiscovered(t *testing.T) {
	d := &daemon{units: map[string]*unit{}}
	d.Pin("osb-bbbbbbbbbbbb", true)

	if !d.isPinned("osb-bbbbbbbbbbbb") || d.isPinned("osb-cccccccccccc") {
		t.Fatal("pin not recorded per sandbox")
	}
}
