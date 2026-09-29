package daemon

import (
	"context"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A unit sleeps within about a second of its window, whatever the window is. The reaper used to
// run every third of the shortest window in force (1s to 30s), so "idle": "30s" slept 37-40s after
// its last byte, and a 5m window up to 30s late: the precision of the setting shrank as it grew.
func TestIdleSleepLandsWithinASecondOfTheWindow(t *testing.T) {
	log.SetOutput(io.Discard)

	for _, c := range []struct {
		idle   string
		window time.Duration
	}{
		{"3s", 3 * time.Second},
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
	} {
		t.Run(c.idle, func(t *testing.T) {
			t.Parallel()

			p := &slowStop{
				listingProvider: listingProvider{units: []provider.Unit{
					{Ref: "r3-" + c.idle, Sandbox: "r3", Service: "s", Running: true, Idle: c.idle,
						Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
				}},
				release: make(chan struct{}),
				stopped: map[string]time.Time{},
			}
			defer close(p.release)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// The daemon's own window is an hour, so only the service's own can make it sleep.
			d := New(p, time.Hour, time.Second, time.Hour)
			d.discover(ctx)

			d.mu.Lock()
			u := d.units["r3-"+c.idle]
			d.mu.Unlock()

			u.mu.Lock()
			u.served = true
			u.mu.Unlock()

			// Due 200ms from now, whatever the window: only the reaper's clock decides how late.
			last := time.Now().Add(-(c.window - 200*time.Millisecond))
			u.lastByte.Store(last.UnixNano())
			due := last.Add(c.window)

			go d.Run(ctx)

			const promise = time.Second + 300*time.Millisecond // one tick, plus scheduling slack

			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if at, ok := p.stopAt("r3-" + c.idle); ok {
					if at.Before(due) {
						t.Fatalf("idle %s: stopped %s BEFORE its window ran out", c.idle, due.Sub(at))
					}

					if late := at.Sub(due); late > promise {
						t.Fatalf("idle %s: stopped %s after its window, want <= %s", c.idle, late, promise)
					}

					return
				}

				time.Sleep(20 * time.Millisecond)
			}

			t.Fatalf("idle %s: not stopped within 5s of its window running out (promise %s)", c.idle, promise)
		})
	}
}

// countingHealth counts every provider call the reaper could make on a tick.
type countingHealth struct {
	listingProvider

	healthy atomic.Int32
	lists   atomic.Int32
	probes  atomic.Int32
	stops   atomic.Int32
}

func (p *countingHealth) Healthy(context.Context, string) (bool, bool) {
	p.healthy.Add(1)
	return false, true // declared and not passing: the unit is never marked served
}

func (p *countingHealth) Probe(context.Context, string) (bool, bool) {
	p.probes.Add(1)
	return false, true
}

func (p *countingHealth) List(ctx context.Context, s string) ([]provider.Unit, error) {
	p.lists.Add(1)
	return p.listingProvider.List(ctx, s)
}

func (p *countingHealth) Stop(context.Context, string) error {
	p.stops.Add(1)
	return nil
}

// A tick that runs every second has to cost nothing but memory. A unit that has served is
// decided from its last-byte clock alone; a unit that never has is asked for its health, which is
// an Engine API call, and that is asked no more often than the old cadence (a third of its
// window) - a second-by-second reaper must not turn into a second-by-second docker inspect of
// every unhealthy container on the machine.
func TestReaperTicksCallTheProviderNoMoreThanBefore(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingHealth{listingProvider: listingProvider{units: []provider.Unit{
		{Ref: "r3-served", Sandbox: "r3", Service: "served", Running: true, Idle: "30s",
			Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
		{Ref: "r3-unhealthy", Sandbox: "r3", Service: "unhealthy", Running: true, Idle: "30s",
			Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
	}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := New(p, time.Hour, time.Second, time.Hour) // discovery never ticks again in this test
	d.discover(ctx)

	d.mu.Lock()
	served := d.units["r3-served"]
	d.mu.Unlock()

	served.mu.Lock()
	served.served = true
	served.mu.Unlock()
	served.touch()

	listsBefore := p.lists.Load()

	go d.Run(ctx)

	time.Sleep(3500 * time.Millisecond) // three or four ticks at one a second
	cancel()

	// Run's own discovery at start is the one List it may make.
	if got := p.lists.Load() - listsBefore; got > 1 {
		t.Errorf("List called %d times in 3.5s; the reaper tick must not list", got)
	}

	// The old cadence for a 30s window was 10s: at most one health ask in 3.5s.
	if got := p.healthy.Load(); got > 1 {
		t.Errorf("Healthy asked %d times in 3.5s for one never-served unit with a 30s window, want <= 1", got)
	}

	if got := p.probes.Load(); got != 0 {
		t.Errorf("Probe called %d times by the reaper", got)
	}

	if got := p.stops.Load(); got != 0 {
		t.Errorf("a unit was stopped %d times inside its 30s window", got)
	}
}

// BenchmarkReapTick is the cost of one reaper decision over a large fleet with nothing due: the
// work a one-second tick adds when nothing is sleeping.
func BenchmarkReapTick(b *testing.B) {
	log.SetOutput(io.Discard)

	d := New(&countingHealth{}, time.Hour, time.Second, time.Hour)

	for i := range 500 {
		ref := "bench-" + string(rune('a'+i%26)) + "-" + time.Duration(i).String()
		u := newUnit("bench", ref, ref, ref, ref, nil, true)
		u.served = true
		u.touch()
		d.units[ref] = u
	}

	ctx := context.Background()

	b.ResetTimer()

	for range b.N {
		d.reap(ctx)
	}
}
