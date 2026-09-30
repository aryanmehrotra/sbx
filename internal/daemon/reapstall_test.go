package daemon

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// slowStop is a provider whose Stop of one ref blocks until released, the way docker's does for
// the full 10s grace on a workload that ignores SIGTERM (a busybox `sh -c` loop). It records when
// each Stop was asked for.
type slowStop struct {
	listingProvider

	block   string
	release chan struct{}

	mu      sync.Mutex
	stopped map[string]time.Time
}

func (p *slowStop) Stop(ctx context.Context, ref string) error {
	p.mu.Lock()
	p.stopped[ref] = time.Now()
	p.mu.Unlock()

	if ref == p.block {
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	}

	return nil
}

func (p *slowStop) stopAt(ref string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	at, ok := p.stopped[ref]

	return at, ok
}

// One slow stop must not make every other service late. The reaper ran each stop inline on the
// loop that also ticks, so a workload taking docker's whole 10s grace held every other due unit
// for those 10s: a service set to "idle": "3s" slept 14s after its last byte, three runs in a row.
// SPEC promises a service sleeps at most one check after its window, and that has to hold while
// some other sandbox is being slow to stop.
func TestASlowStopDoesNotDelayAnotherUnitsSleep(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &slowStop{
		listingProvider: listingProvider{units: []provider.Unit{
			{Ref: "r2-slow", Sandbox: "r2", Service: "slow", Running: true, Idle: "1s",
				Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
			{Ref: "r2-fast", Sandbox: "r2", Service: "fast", Running: true, Idle: "1s",
				Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
		}},
		block:   "r2-slow",
		release: make(chan struct{}),
		stopped: map[string]time.Time{},
	}
	defer close(p.release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := New(p, time.Hour, time.Second, time.Hour)
	d.discover(ctx)

	d.mu.Lock()
	slow, fast := d.units["r2-slow"], d.units["r2-fast"]
	d.mu.Unlock()

	// Both already seen serving, so the first-serve grace does not blur the timing. The slow one
	// has been idle for an hour and is stopped on the first tick; the fast one goes idle from now.
	for _, u := range []*unit{slow, fast} {
		u.mu.Lock()
		u.served = true
		u.mu.Unlock()
	}

	slow.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())
	fast.touch()

	go d.Run(ctx)

	// The fast unit's last byte lands between the first tick and the second, so it falls due
	// on a later tick than the slow one - the tick an inline stop is still blocking.
	time.Sleep(500 * time.Millisecond)
	fast.touch()

	lastByte := time.Now()

	// Window 1s, one reapTick (1s): due by 2s after its last byte, plus scheduling slack.
	const promise = 2*time.Second + 500*time.Millisecond

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if at, ok := p.stopAt("r2-fast"); ok {
			if late := at.Sub(lastByte); late > promise {
				t.Fatalf("fast unit stopped %s after its last byte, want <= %s", late, promise)
			}

			if _, ok := p.stopAt("r2-slow"); !ok {
				t.Fatal("the slow unit was never stopped, so this proved nothing about a stall")
			}

			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("fast unit (idle 1s) not stopped within 6s while another unit's stop was blocking")
}

// countingStop blocks every Stop until released and counts them.
type countingStop struct {
	alwaysServing

	entered chan string
	release chan struct{}

	mu    sync.Mutex
	stops map[string]int
}

func (p *countingStop) Stop(_ context.Context, ref string) error {
	p.mu.Lock()
	p.stops[ref]++
	p.mu.Unlock()

	p.entered <- ref
	<-p.release

	return nil
}

func (p *countingStop) count(ref string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.stops[ref]
}

func idleServed(sandbox, service string) *unit {
	u := newUnit(sandbox, service, service, "", service, nil, true)
	u.served = true
	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

	return u
}

// With stops off the reaper's goroutine, ticks keep arriving while a sleep waits for the wake
// lock - behind a wake that is failing, or a discovery check re-asking the provider, neither of
// which touches the unit. Each tick must not queue another sleep: once the lock frees, every
// queued one still sees the unit idle and stops it again.
func TestTicksWhileASleepWaitsDoNotStopTheUnitAgain(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingStop{entered: make(chan string, 8), release: make(chan struct{}), stops: map[string]int{}}
	close(p.release)

	u := idleServed("r2", "s")

	d := &daemon{provider: p, idle: time.Minute, units: map[string]*unit{"s": u}}

	u.waking.Lock() // something else is driving the unit

	ticks := make([]*sync.WaitGroup, 0, 3)
	for range 3 {
		ticks = append(ticks, d.reapAsync(context.Background()))
	}

	u.waking.Unlock()

	for _, wg := range ticks {
		wg.Wait()
	}

	if n := p.count("s"); n != 1 {
		t.Fatalf("one idle unit was stopped %d times across three ticks, want 1", n)
	}
}

// A dependent that is mid-stop still needs its dependency. sleep() marks it asleep at the start
// of a stop that can take ten seconds, and a graceful shutdown may be flushing to that database;
// the inline reaper never let the next tick see that state, so the order came for free.
func TestADependencyWaitsForItsDependentsStopToFinish(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingStop{entered: make(chan string, 4), release: make(chan struct{}), stops: map[string]int{}}

	app := idleServed("r2", "app")
	app.dependsOn = []string{"db"}
	db := idleServed("r2", "db")

	d := &daemon{provider: p, idle: time.Minute, units: map[string]*unit{"app": app, "db": db}}

	first := d.reapAsync(context.Background())

	if ref := <-p.entered; ref != "app" {
		t.Fatalf("first stop was %q, want app: its dependency is needed while it is up", ref)
	}

	// The next tick, while app is still going down. A stop of db would block like app's, so
	// look for it arriving rather than waiting for it to finish.
	second := d.reapAsync(context.Background())

	select {
	case ref := <-p.entered:
		close(p.release)
		t.Fatalf("%s was stopped while app, which depends on db, was still stopping", ref)
	case <-time.After(300 * time.Millisecond):
	}

	close(p.release)
	first.Wait()
	second.Wait()

	// Once app is down, db goes on the tick after.
	d.reap(context.Background())

	if n := p.count("db"); n != 1 {
		t.Fatalf("db stopped %d times once its dependent was down, want 1", n)
	}
}
