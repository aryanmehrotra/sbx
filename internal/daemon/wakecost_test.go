package daemon

// What a cold wake costs in work.
//
// With a caveat this file exists to record. Counting calls is deterministic where wall-clock
// on a loaded machine is not, but it answers a different question: it treats every call as
// free. A backoff from 5 ms looked like a large win by this measure - an 8 ms workload
// declared awake in 102 ms flat against about 20 ms backing off - and was worth nothing
// end-to-end, because each probe is an Engine API exec and probing at 5 ms cannot sample
// faster than the probe costs. Measured, order alternating, n=14: 162 ms flat, 166 ms
// backing off.
//
// So what is asserted here is the change that is less work by construction and cannot be
// paid for elsewhere: not probing before starting a container that is stopped.

import (
	"context"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"
)

// readyAfter is a workload that becomes serving once, after a delay - the shape of a real
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

// Nothing is asked before the container is started.
//
// The unit being asleep is why wake was called, so a probe before Start could only fail -
// and starting an already-running container is a 304 the provider treats as success, so the
// case that probe existed for costs nothing without it. One round trip per cold wake.
func TestAColdWakeDoesNotProbeBeforeStarting(t *testing.T) {
	log.SetOutput(io.Discard)

	// Ready the moment it starts, so the poll loop needs exactly one probe. Any second probe
	// is the pre-Start one, which is what this test is for.
	p := &readyAfter{delay: 0}
	u := newUnit("s", "svc", "ref", "inst-ref", "s/svc", nil, false)

	if err := u.wake(context.Background(), p, 10*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	if p.starts.Load() != 1 {
		t.Fatalf("the container was started %d times, want once", p.starts.Load())
	}

	// One probe: the poll loop's first iteration, after Start. Two would mean the pre-Start
	// probe is back.
	if got := p.probes.Load(); got != 1 {
		t.Errorf("a cold wake cost %d probes, want 1 - each is a round trip to the runtime, "+
			"and a probe before Start can only fail", got)
	}
}

// And a workload that takes a while is not hammered for the whole of it: the interval bounds
// how much traffic a slow start costs.
func TestASlowWorkloadIsNotProbedHundredsOfTimes(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &readyAfter{delay: 900 * time.Millisecond}
	u := newUnit("s", "svc", "ref", "inst-ref", "s/svc", nil, false)

	if err := u.wake(context.Background(), p, 20*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	if got := p.probes.Load(); got > 15 {
		t.Errorf("a 900 ms wake cost %d probes at a 100 ms interval - that is more than the "+
			"interval allows, so something is polling faster than it says", got)
	}
}
