package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// pausing records which verb the daemon used, because the whole point of a freeze is that it is
// NOT a stop: a stop loses the processes the caller wanted kept.
type pausing struct {
	listingProvider

	mu    sync.Mutex
	calls []string
}

func (p *pausing) note(s string) {
	p.mu.Lock()
	p.calls = append(p.calls, s)
	p.mu.Unlock()
}

func (p *pausing) seen() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return strings.Join(p.calls, ",")
}

func (p *pausing) Start(_ context.Context, ref string) error   { p.note("start " + ref); return nil }
func (p *pausing) Stop(_ context.Context, ref string) error    { p.note("stop " + ref); return nil }
func (p *pausing) Pause(_ context.Context, ref string) error   { p.note("pause " + ref); return nil }
func (p *pausing) Unpause(_ context.Context, ref string) error { p.note("unpause " + ref); return nil }
func (p *pausing) Probe(context.Context, string) (bool, bool) {
	p.note("probe")
	return true, true
}

func idleUnit(freeze bool) *unit {
	u := newUnit("osb-1", "sandbox", "c1", "i1", "c1", nil, true)
	u.freezeOnIdle = freeze
	u.served = true
	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

	return u
}

func TestIdleFreezesInsteadOfStoppingWhenAsked(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &pausing{}
	u := idleUnit(true)

	d := &daemon{provider: p, idle: time.Minute, units: map[string]*unit{"c1": u}}
	d.reap(context.Background())

	if got := p.seen(); got != "pause c1" {
		t.Fatalf("idle unit asking to freeze got %q, want only a pause", got)
	}

	if u.isAwake() || !u.isFrozen() {
		t.Fatalf("after freezing: awake=%v frozen=%v, want asleep and frozen", u.isAwake(), u.isFrozen())
	}
}

// The default must not move: every sandbox from a sandbox.json still stops to 0 B.
func TestIdleStillStopsByDefault(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &pausing{}
	d := &daemon{provider: p, idle: time.Minute, units: map[string]*unit{"c1": idleUnit(false)}}
	d.reap(context.Background())

	if got := p.seen(); got != "stop c1" {
		t.Fatalf("default idle got %q, want a stop", got)
	}
}

// A frozen container cannot be started - docker refuses - and needs no health probe either:
// its processes resume where they were.
func TestAFrozenUnitIsThawedNotStarted(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &pausing{}
	u := idleUnit(true)
	u.setAwake(false)
	u.setFrozen(true)

	if err := u.wake(context.Background(), p, time.Second); err != nil {
		t.Fatal(err)
	}

	if got := p.seen(); got != "unpause c1" {
		t.Fatalf("waking a frozen unit did %q, want a single unpause and no probe", got)
	}

	if !u.isAwake() || u.isFrozen() {
		t.Fatalf("after thaw: awake=%v frozen=%v", u.isAwake(), u.isFrozen())
	}
}

func TestPausedOnPurposeIsNotThawedByTraffic(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &pausing{}
	u := idleUnit(true)
	u.lastByte.Store(time.Now().UnixNano())

	d := &daemon{provider: p, ready: time.Second, units: map[string]*unit{"c1": u},
		stop: map[string]context.CancelFunc{}}

	if err := d.Freeze(context.Background(), "osb-1"); err != nil {
		t.Fatal(err)
	}

	// A connection now: it must be hung up on, and nothing started or unpaused.
	client, server := net.Pipe()
	done := make(chan struct{})

	go func() {
		u.handle(context.Background(), p, server, leg{Upstream: provider.Endpoint{Host: "127.0.0.1", Port: 1}}, time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a connection to a paused sandbox was held open instead of refused")
	}

	_ = client.Close()

	var held errHeld
	if err := u.wake(context.Background(), p, time.Second); !errors.As(err, &held) {
		t.Fatalf("wake of a held unit = %v, want errHeld", err)
	}

	if got := p.seen(); got != "pause c1" {
		t.Fatalf("while held the provider saw %q, want only the pause", got)
	}

	if err := d.Thaw(context.Background(), "osb-1"); err != nil {
		t.Fatal(err)
	}

	if got := p.seen(); got != "pause c1,unpause c1" {
		t.Fatalf("resume did %q, want an unpause", got)
	}
}

// failingPause is a provider whose pause and unpause fail, the way a docker call does when the
// API request that asked for it is cancelled half way.
type failingPause struct{ pausing }

func (*failingPause) Pause(context.Context, string) error   { return context.Canceled }
func (*failingPause) Unpause(context.Context, string) error { return context.Canceled }

// Start fails too: a failed unpause falls back to a start, and the thaw only fails when both do.
func (*failingPause) Start(context.Context, string) error { return context.Canceled }

// A pause that failed must leave nothing held. The API answers 500 and keeps the sandbox
// Running, and resume refuses a sandbox that is not paused - so a hold left behind here would
// hang up on every connection with no call that could ever release it.
func TestAFailedFreezeHoldsNothing(t *testing.T) {
	log.SetOutput(io.Discard)

	u := idleUnit(true)
	d := &daemon{provider: &failingPause{}, units: map[string]*unit{"c1": u}}

	if err := d.Freeze(context.Background(), "osb-1"); err == nil {
		t.Fatal("Freeze with a failing pause returned nil")
	}

	if u.isHeld() || d.held["osb-1"] {
		t.Fatalf("after a failed freeze: unit held=%v, daemon held=%v; want neither", u.isHeld(), d.held["osb-1"])
	}
}

// The mirror: a resume that failed leaves the API saying Paused, so the daemon must go on
// refusing traffic, or "a paused sandbox is not woken by a connection" stops being true for it.
func TestAFailedThawKeepsTheHold(t *testing.T) {
	log.SetOutput(io.Discard)

	u := idleUnit(true)
	u.setAwake(false)
	u.setFrozen(true)
	u.held.Store(true)

	d := &daemon{provider: &failingPause{}, ready: time.Second, units: map[string]*unit{"c1": u},
		held: map[string]bool{"osb-1": true}}

	if err := d.Thaw(context.Background(), "osb-1"); err == nil {
		t.Fatal("Thaw with a failing unpause returned nil")
	}

	if !u.isHeld() || !d.held["osb-1"] {
		t.Fatalf("after a failed thaw: unit held=%v, daemon held=%v; want both still held", u.isHeld(), d.held["osb-1"])
	}
}

// Pausing a backend that cannot keep memory must be refused, not approximated with a stop.
func TestFreezeIsRefusedByAProviderThatCannotPause(t *testing.T) {
	log.SetOutput(io.Discard)

	d := &daemon{provider: &listingProvider{}, units: map[string]*unit{}}

	err := d.Freeze(context.Background(), "osb-1")
	if err == nil || !strings.Contains(err.Error(), "cannot pause") {
		t.Fatalf("Freeze on a provider without Pauser = %v, want a refusal", err)
	}
}

// A pause done outside sbx is only visible to discovery, and without it the next wake asks
// docker to START a paused container, which it refuses.
func TestDiscoveryLearnsThatAUnitIsFrozen(t *testing.T) {
	log.SetOutput(io.Discard)

	u := newUnit("zn", "db", "db", "i-db", "db", nil, true)
	p := &listingProvider{units: []provider.Unit{{Ref: "db", Sandbox: "zn", Service: "db", Paused: true}}}

	d := &daemon{provider: p, units: map[string]*unit{"db": u}, stop: map[string]context.CancelFunc{"db": func() {}}}
	d.discover(context.Background())

	if !u.isFrozen() || u.isAwake() {
		t.Fatalf("after discovering a paused container: frozen=%v awake=%v", u.isFrozen(), u.isAwake())
	}
}
