package daemon

// What a cold wake costs in work, rather than in wall-clock milliseconds.
//
// Wall-clock on a loaded machine says nothing — measured during this work at load average 9,
// the same wake ranged 883 ms to 3457 ms and an A/B of the two implementations differed by
// less than the noise. Counting the calls is deterministic and says the same thing on any
// machine: a probe is a round trip to the container runtime, so fewer probes is less work
// whatever the hardware is doing.

import (
	"context"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"
)

// readyAfter is a workload that becomes serving once, after a delay — the shape of a real
// container: not ready when Start returns, ready shortly after.
type readyAfter struct {
	countingProvider

	delay   time.Duration
	started atomic.Int64
}

func (r *readyAfter) Start(_ context.Context, _ string) error {
	r.starts.Add(1)
	r.started.Store(time.Now().UnixNano())

	return nil
}

func (r *readyAfter) Probe(context.Context, string) (bool, bool) {
	r.probes.Add(1)

	at := r.started.Load()
	if at == 0 {
		return false, true // not started yet, so certainly not serving
	}

	return time.Since(time.Unix(0, at)) >= r.delay, true
}

// A workload that is ready almost immediately must not be rounded up to a poll interval, and
// must not be probed before it has even been started.
func TestAFastWorkloadIsNotRoundedUpToThePollInterval(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &readyAfter{delay: 8 * time.Millisecond}
	u := newUnit("s", "svc", "ref", "s/svc", nil, false)

	start := time.Now()

	if err := u.wake(context.Background(), p, 10*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	took := time.Since(start)

	// With a flat 100 ms poll this could not come in under 100 ms; with a backoff from 5 ms
	// it should land close to the workload's own 8 ms. Generous, because CI machines are
	// slow, and still far below the interval it replaced.
	if took > 60*time.Millisecond {
		t.Errorf("an 8 ms workload took %s to be declared awake — the poll interval is being "+
			"paid rather than the workload", took.Round(time.Millisecond))
	}

	// And nothing was asked before the container was started: that probe could only ever
	// fail, because the unit being asleep is why wake was called.
	if p.probes.Load() > 3 {
		t.Errorf("a cold wake cost %d probes; each is a round trip to the runtime", p.probes.Load())
	}
}

// The other end: a workload that takes a while must not be hammered for the whole of it.
func TestASlowWorkloadIsNotProbedHundredsOfTimes(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &readyAfter{delay: 900 * time.Millisecond}
	u := newUnit("s", "svc", "ref", "s/svc", nil, false)

	if err := u.wake(context.Background(), p, 20*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	// 5,10,20,40,80,100,100… over 900 ms is on the order of a dozen. A backoff that never
	// grew would be 180 at 5 ms flat.
	if got := p.probes.Load(); got > 25 {
		t.Errorf("a 900 ms wake cost %d probes — the backoff is not growing", got)
	}
}
