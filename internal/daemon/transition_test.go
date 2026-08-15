package daemon

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// sleepAndWakeMustNotOverlap
//
// The reaper decides to sleep a unit; a connection arrives while the stop is still in
// flight. Both paths then drive the same container in opposite directions, and the client
// is the one that finds out: it is handed a connection to something that is being stopped
// underneath it.
//
// wake() already serialises against itself with u.waking. sleep() did not take that lock at
// all, so "is anything else touching this container" was only ever half asked.
//
// This records the order of provider calls and asserts the obvious invariant: no start may
// begin between the beginning and the end of a stop.

type transitionRecorder struct {
	mu      sync.Mutex
	events  []string
	serving bool

	stopFor time.Duration // how long a stop takes, to make the window real
}

func (r *transitionRecorder) record(ev string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, ev)
}

func (r *transitionRecorder) Start(context.Context, string) error {
	r.record("start")

	r.mu.Lock()
	r.serving = true
	r.mu.Unlock()

	return nil
}

func (r *transitionRecorder) Stop(context.Context, string) error {
	r.record("stop-begin")

	time.Sleep(r.stopFor) // a real docker stop is not instant

	r.mu.Lock()
	r.serving = false
	r.mu.Unlock()

	r.record("stop-end")

	return nil
}

func (r *transitionRecorder) Probe(context.Context, string) (bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.serving, true
}

func (r *transitionRecorder) Healthy(ctx context.Context, ref string) (bool, bool) {
	return r.Probe(ctx, ref)
}

func (r *transitionRecorder) seq() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.events...)
}

// Everything below is the rest of the Provider surface, unused here.
func (*transitionRecorder) Name() string { return "transition" }
func (*transitionRecorder) AllocSlot(context.Context, string) (int, error) {
	return 0, nil
}

func (*transitionRecorder) Endpoints(_, _ string, _, _ int, _ []int) []provider.Endpoint {
	return nil
}

func (*transitionRecorder) Create(context.Context, string, int, int, string,
	spec.Service, []provider.Endpoint, string, provider.Isolation,
) error {
	return nil
}
func (*transitionRecorder) List(context.Context, string) ([]provider.Unit, error) { return nil, nil }
func (*transitionRecorder) Remove(context.Context, string) error                  { return nil }
func (*transitionRecorder) Exec(context.Context, string, []string) (string, error) {
	return "", nil
}
func (*transitionRecorder) Commit(context.Context, string, string) error { return nil }

func (*transitionRecorder) Images(context.Context, string) ([]string, error) { return nil, nil }

func (*transitionRecorder) CopyVolume(context.Context, string, string) error { return nil }

func (*transitionRecorder) VolumeFor(string, string) string { return "" }

func (*transitionRecorder) ExecTTY(context.Context, string, []string) error { return nil }
func (*transitionRecorder) Logs(context.Context, string, int, bool, io.Writer) error {
	return nil
}
func (*transitionRecorder) Copy(context.Context, string, string, string) error { return nil }

func TestSleepAndWakeDoNotOverlap(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &transitionRecorder{serving: true, stopFor: 200 * time.Millisecond}

	u := newUnit("t", "svc", "ref", "t", nil, true)
	u.setAwake(true)

	u.mu.Lock()
	u.served = true // it has been seen serving, so the reaper is allowed to sleep it
	u.mu.Unlock()

	// Idle long enough that the reaper wants it gone.
	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

	d := &daemon{
		provider: p,
		idle:     time.Millisecond,
		ready:    5 * time.Second,
		refresh:  time.Hour,
		units:    map[string]*unit{"ref": u},
		stop:     map[string]context.CancelFunc{},
	}

	ctx := context.Background()

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()
		d.reap(ctx)
	}()

	// Arrive in the middle of the stop, which is what a client does.
	time.Sleep(60 * time.Millisecond)

	wg.Add(1)

	go func() {
		defer wg.Done()

		if err := u.wake(ctx, p, 5*time.Second); err != nil {
			t.Errorf("wake: %v", err)
			return
		}

		// The moment wake returns, the caller believes the sandbox is serving and dials it.
		// If that happens inside a stop, the caller is dialling something being torn down.
		p.record("wake-returned-ok")
	}()

	wg.Wait()

	seq := p.seq()

	inStop := false

	for _, ev := range seq {
		switch ev {
		case "stop-begin":
			inStop = true
		case "stop-end":
			inStop = false
		case "start":
			if inStop {
				t.Fatalf("a start ran while a stop was in flight.\n  sequence: %v", seq)
			}
		case "wake-returned-ok":
			if inStop {
				t.Fatalf("wake told a caller the sandbox was serving while a stop was in "+
					"flight — that caller now dials a container being torn down.\n"+
					"  sequence: %v", seq)
			}
		}
	}

	// And the unit must end up awake: a wake that arrived during a sleep has to win, or the
	// caller waited for nothing and the sandbox is left down.
	if !u.isAwake() {
		t.Errorf("after a wake raced a sleep the unit is not awake.\n  sequence: %v", seq)
	}
}
