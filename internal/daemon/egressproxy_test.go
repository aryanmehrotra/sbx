package daemon

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The throttle in front of the unit-map walk. The filter itself lives in internal/egress and is
// tested there; what is tested here is the daemon's side of the hook - how often a busy stream is
// allowed to take the lock the wake path also takes.

func TestDueThrottlesToOneWalkASecond(t *testing.T) {
	p := &egressProxy{}
	now := time.Now().UnixNano()

	if !p.due(now) {
		t.Fatal("the first stamp must go through")
	}

	if p.due(now + int64(100*time.Millisecond)) {
		t.Fatal("a second stamp 100ms later must be skipped: a download would otherwise take " +
			"the daemon lock once per chunk")
	}

	if !p.due(now + int64(2*time.Second)) {
		t.Fatal("a stamp two seconds later must go through")
	}
}

func TestDueIsRaceFreeAndAdmitsOne(t *testing.T) {
	p := &egressProxy{}
	now := time.Now().UnixNano()

	var (
		wg     sync.WaitGroup
		passed atomic.Int64
	)

	for range 64 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if p.due(now) {
				passed.Add(1)
			}
		}()
	}

	wg.Wait()

	if got := passed.Load(); got != 1 {
		t.Fatalf("%d of 64 concurrent stamps passed, want exactly 1", got)
	}
}

func BenchmarkEgressDueThrottled(b *testing.B) {
	p := &egressProxy{}

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = p.due(1)
	}
}

// TestAContainerFilteredGatewayIsStampedWithoutAProxy is the regression for a bug the unit tests
// could not see and a live sandbox found immediately: a box calling out every five seconds slept
// anyway.
//
// touchEgress looked up d.egress[gw] to find its throttle, and a gateway whose filter runs as a
// CONTAINER has no entry there by design - reconcileEgress skips it precisely so the daemon does
// not also try to bind it. The lookup failed, the function returned, and the walk that does the
// stamping was never reached. The throttle exists for the in-process filter, which calls this per
// copied chunk; a scrape already arrives at most once a tick and needs no throttling at all.
func TestAContainerFilteredGatewayIsStampedWithoutAProxy(t *testing.T) {
	d := &daemon{
		units:      map[string]*unit{},
		egress:     map[string]*egressProxy{}, // deliberately empty: the filter is a container
		egressSeen: map[string]int64{},
	}

	u := &unit{egressGateway: "172.19.0.1"}
	u.lastByte.Store(1)
	d.units["ref"] = u

	d.touchEgress("172.19.0.1")

	if u.lastByte.Load() <= 1 {
		t.Fatal("a gateway whose filter is a container was not stamped: a box that only calls " +
			"out sleeps mid-task, which is the whole bug this feature exists to fix")
	}
}

// And the in-process filter must still be throttled, or a download takes the daemon lock once
// per chunk.
func TestAnInProcessGatewayIsStillThrottled(t *testing.T) {
	d := &daemon{
		units:      map[string]*unit{},
		egress:     map[string]*egressProxy{"gw": {}},
		egressSeen: map[string]int64{},
	}

	u := &unit{egressGateway: "gw"}
	d.units["ref"] = u

	d.touchEgress("gw") // first one goes through and claims the second

	first := u.lastByte.Load()
	if first == 0 {
		t.Fatal("the first stamp did not land")
	}

	u.lastByte.Store(0)
	d.touchEgress("gw") // immediately after: throttled

	if u.lastByte.Load() != 0 {
		t.Fatal("a second stamp inside the throttle window reached the unit map")
	}
}
