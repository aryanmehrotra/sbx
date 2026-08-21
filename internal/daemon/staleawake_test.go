package daemon

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// listingProvider reports a fixed fleet, so a test can say what the world looks like and
// check what the daemon concluded from it.
type listingProvider struct {
	alwaysServing

	units []provider.Unit
}

func (l *listingProvider) List(context.Context, string) ([]provider.Unit, error) {
	return l.units, nil
}

// The daemon's belief that a unit is awake must be corrected by what the provider reports,
// not only by a dial that fails.
//
// wake() returns immediately for a unit it believes is already up. For the service a
// connection addressed that optimism is self-correcting: the dial afterwards fails and the
// belief is revoked. A dependency is never dialled - it is woken so that something else can
// reach it over the sandbox network - so nothing corrects it there, and a container stopped
// out of band stays "awake" forever while being absent from DNS. That is the failure this
// whole feature exists to prevent, arrived at from the other side.
func TestDiscoverCorrectsAStaleAwakeBelief(t *testing.T) {
	log.SetOutput(io.Discard)

	u := newUnit("zn", "db", "db", "i-db", "db", nil, true)
	if !u.isAwake() {
		t.Fatal("unit should start awake for this test")
	}

	p := &listingProvider{units: []provider.Unit{{Ref: "db", Sandbox: "zn", Service: "db", Running: false}}}

	d := &daemon{provider: p, units: map[string]*unit{u.name: u}, stop: map[string]context.CancelFunc{
		u.name: func() {},
	}}

	d.discover(context.Background())

	if u.isAwake() {
		t.Error("still believed awake after the provider reported it stopped")
	}
}

// The correction only ever goes one way here. A unit the provider reports as running must not
// be flipped awake by discovery: wake() is what establishes that, under the lock that
// serialises it against sleep, and a tick racing in from the side would make "awake" mean
// nothing.
func TestDiscoverDoesNotMarkAwakeOnItsOwn(t *testing.T) {
	log.SetOutput(io.Discard)

	u := newUnit("zn", "db", "db", "i-db", "db", nil, false)

	p := &listingProvider{units: []provider.Unit{{Ref: "db", Sandbox: "zn", Service: "db", Running: true}}}

	d := &daemon{provider: p, units: map[string]*unit{u.name: u}, stop: map[string]context.CancelFunc{
		u.name: func() {},
	}}

	d.discover(context.Background())

	if u.isAwake() {
		t.Error("discovery marked a unit awake; only wake() may do that")
	}
}

// A wake in flight must not have its belief torn down by a tick landing mid-start.
func TestDiscoverLeavesAWakeInFlightAlone(t *testing.T) {
	log.SetOutput(io.Discard)

	u := newUnit("zn", "db", "db", "i-db", "db", nil, true)

	u.waking.Lock() // stand in for a wake in progress
	defer u.waking.Unlock()

	p := &listingProvider{units: []provider.Unit{{Ref: "db", Sandbox: "zn", Service: "db", Running: false}}}

	d := &daemon{provider: p, units: map[string]*unit{u.name: u}, stop: map[string]context.CancelFunc{
		u.name: func() {},
	}}

	done := make(chan struct{})
	go func() { d.discover(context.Background()); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("discover blocked on a unit that was waking")
	}
}
