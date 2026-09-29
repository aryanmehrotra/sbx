package daemon

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// stateful is a provider that remembers whether its one container is running, so a test can
// check the daemon's belief against the truth.
//
// Its first List takes the snapshot, says so on listed, and does not return until release is
// closed - a listing that was true when docker answered and is stale by the time discover acts
// on it. Every later List answers from the current state.
type stateful struct {
	alwaysServing

	mu      sync.Mutex
	running bool
	stops   int
	lists   int

	listed  chan struct{}
	release chan struct{}

	// stopEntered and stopRelease, when set, make Stop block after the container has been
	// asked to stop and before the call returns - docker's grace period, held open by the test.
	stopEntered chan struct{}
	stopRelease chan struct{}
}

func (p *stateful) List(context.Context, string) ([]provider.Unit, error) {
	p.mu.Lock()
	snap := p.running
	first := p.lists == 0
	p.lists++
	p.mu.Unlock()

	if first && p.listed != nil {
		close(p.listed)
		<-p.release
	}

	return []provider.Unit{{Ref: "s", Sandbox: "r2", Service: "s", Running: snap}}, nil
}

func (p *stateful) Start(context.Context, string) error {
	p.mu.Lock()
	p.running = true
	p.mu.Unlock()

	return nil
}

func (p *stateful) Stop(context.Context, string) error {
	if p.stopEntered != nil {
		close(p.stopEntered)
		<-p.stopRelease
	}

	p.mu.Lock()
	p.running = false
	p.stops++
	p.mu.Unlock()

	return nil
}

func (p *stateful) state() (running bool, stops int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.running, p.stops
}

// A listing taken before a wake must not revoke that wake.
//
// This is the service that stayed up for two minutes with `"idle": "3s"`. The reaper's stop ran
// on the discovery loop, so the discovery tick due during it fired the moment it returned and
// listed the container as exited - while the connection that had arrived mid-stop was already
// starting it again. The wake finished (woke in 175ms), and 17ms later discover applied its stale
// "not running" and marked the unit asleep: "was stopped outside sbx". The container was up, the
// daemon believed it down, and the reaper only ever considers units it believes awake - so
// nothing slept it again until some client happened to connect.
func TestAStaleListingDoesNotRevokeAWakeThatFinishedAfterIt(t *testing.T) {
	log.SetOutput(io.Discard)

	u := newUnit("r2", "s", "s", "", "s", nil, false)
	u.served = true

	p := &stateful{listed: make(chan struct{}), release: make(chan struct{})}

	d := &daemon{provider: p, idle: time.Hour, units: map[string]*unit{"s": u},
		stop: map[string]context.CancelFunc{"s": func() {}}}

	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		d.discover(ctx)
		close(done)
	}()

	<-p.listed // the snapshot says stopped

	if err := u.wake(ctx, p, time.Second); err != nil {
		t.Fatal(err)
	}

	close(p.release)
	<-done

	if running, _ := p.state(); !running || !u.isAwake() {
		t.Fatalf("after a wake that finished after the listing: running=%v, believed awake=%v - "+
			"the daemon disagrees with the container", running, u.isAwake())
	}

	// And the reaper considers it again: idle past the window, it is stopped.
	u.lastByte.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	d.reap(ctx)

	if _, stops := p.state(); stops != 1 {
		t.Fatalf("an idle unit the daemon had woken was stopped %d times by the reaper, want 1", stops)
	}
}

// The listing still corrects a belief the provider genuinely contradicts. The fix above re-asks
// rather than ignores, so a container stopped outside sbx is still caught.
func TestAFreshListingStillRevokesAStaleAwake(t *testing.T) {
	log.SetOutput(io.Discard)

	u := newUnit("r2", "s", "s", "", "s", nil, true)
	p := &stateful{running: false}

	d := &daemon{provider: p, units: map[string]*unit{"s": u}, stop: map[string]context.CancelFunc{"s": func() {}}}
	d.discover(context.Background())

	if u.isAwake() {
		t.Fatal("still believed awake after the provider, asked twice, reported it stopped")
	}
}

// A connection that lands while the stop is in flight must not corrupt the sleep's own record,
// and must leave the unit where the reaper will find it again.
//
// The slept event used to read the idle time after Stop returned, so a byte during the stop
// was counted against it: "slept - idle for 2s" (1967ms) on a service whose window is 3s - an
// event reporting a sleep the policy never allowed. The idle time is the one observed when the
// decision was made.
func TestAConnectionDuringTheStopKeepsTheSleepRecordAndTheReaper(t *testing.T) {
	log.SetOutput(io.Discard)

	const sandbox = "r2-sleeprace-conn"

	var (
		mu    sync.Mutex
		slept []int64
	)

	logs.Default.Observe(func(e logs.Entry) {
		if e.Sandbox == sandbox && e.Event == "slept" {
			mu.Lock()
			slept = append(slept, e.DurationMs)
			mu.Unlock()
		}
	})

	u := newUnit(sandbox, "s", "s", "", "s", nil, true)
	u.served = true
	u.idle = 3 * time.Second
	u.lastByte.Store(time.Now().Add(-5 * time.Second).UnixNano())

	p := &stateful{running: true, stopEntered: make(chan struct{}), stopRelease: make(chan struct{})}

	d := &daemon{provider: p, idle: time.Hour, units: map[string]*unit{"s": u},
		stop: map[string]context.CancelFunc{"s": func() {}}}

	ctx := context.Background()

	reaped := make(chan struct{})
	go func() {
		d.reap(ctx)
		close(reaped)
	}()

	<-p.stopEntered

	// The connection: handle() stamps it, then waits on the wake lock the stop holds.
	u.touch()

	woke := make(chan error, 1)
	go func() { woke <- u.wake(ctx, p, time.Second) }()

	time.Sleep(50 * time.Millisecond) // the byte is well inside the stop, not after it
	close(p.stopRelease)

	<-reaped

	if err := <-woke; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := append([]int64(nil), slept...)
	mu.Unlock()

	if len(got) != 1 || got[0] < 3000 {
		t.Fatalf("slept events %v ms, want one reporting the >= 3000ms observed when it was decided", got)
	}

	if running, _ := p.state(); !running || !u.isAwake() {
		t.Fatalf("after the mid-stop connection woke it: running=%v, believed awake=%v", running, u.isAwake())
	}

	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())
	p.stopEntered = nil
	d.reap(ctx)

	if _, stops := p.state(); stops != 2 {
		t.Fatalf("the reaper stopped it %d times in all, want 2: the woken unit must be reaped again", stops)
	}
}
