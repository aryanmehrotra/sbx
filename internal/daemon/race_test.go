package daemon

// Concurrency, under the race detector.
//
//	go test -race -run Concurrent
//
// The daemon is the only genuinely concurrent thing here: listeners per port, a goroutine
// per connection, a discovery loop and a reaper all touching the same units. Every previous
// test drove one connection at a time, which is the shape least likely to find anything.
//
// These drive the paths that actually overlap in production — many callers waking one
// sandbox at once, and the reaper deciding to sleep something while a connection is
// arriving — because that is where a data race would be, and where the damage would be a
// sandbox stopped underneath a live query.

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"time"
)

// countingProvider records how often the workload was started, so a burst of connections
// can be asserted to have started it once rather than once each.
type countingProvider struct {
	alwaysServing

	starts  atomic.Int32
	stops   atomic.Int32
	probes  atomic.Int32
	serving atomic.Bool
}

func (c *countingProvider) Start(context.Context, string) error {
	c.starts.Add(1)
	c.serving.Store(true)

	return nil
}

func (c *countingProvider) Stop(context.Context, string) error {
	c.stops.Add(1)
	c.serving.Store(false)

	return nil
}

func (c *countingProvider) Probe(context.Context, string) (bool, bool) {
	// Counted: a probe is a `docker exec` in the real provider, and awake_test.go asserts
	// that connections to an already-awake unit cost none.
	c.probes.Add(1)

	return c.serving.Load(), true
}

func (c *countingProvider) Healthy(context.Context, string) (bool, bool) {
	return c.serving.Load(), true
}

// A burst of simultaneous connections to a sleeping sandbox must start it once. Without the
// wake lock every connection would race to start it, and a database asked to start fifty
// times at once is a database that answers none of them quickly.
func TestConcurrentWakesStartOnce(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	up := echoServer(t)
	port := freePort(t)

	p := &countingProvider{}

	u := newUnit("race", "svc", "ref", "race", nil, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = u.serve(ctx, p, leg{
			Listen:   port,
			Upstream: provider.Endpoint{Host: "127.0.0.1", Port: up.Addr().(*net.TCPAddr).Port},
		}, 10*time.Second)
	}()

	waitForListener(t, port)

	const callers = 40

	var wg sync.WaitGroup

	wg.Add(callers)

	for range callers {
		go func() {
			defer wg.Done()

			c, err := net.DialTimeout("tcp", listenAddr(port), 10*time.Second)
			if err != nil {
				return
			}
			defer c.Close()

			_, _ = c.Write([]byte("x"))

			buf := make([]byte, 1)
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, _ = c.Read(buf)
		}()
	}

	wg.Wait()

	if got := p.starts.Load(); got != 1 {
		t.Errorf("%d callers woke the sandbox %d times, want 1", callers, got)
	}
}

// The reaper and an arriving connection touch the same unit from different goroutines. This
// is the overlap that would stop a sandbox underneath a live query.
func TestConcurrentWakeAndReap(t *testing.T) {
	log.SetOutput(io.Discard)

	up := echoServer(t)
	port := freePort(t)

	p := &countingProvider{}
	p.serving.Store(true)

	u := newUnit("race", "svc", "ref", "race", nil, true)

	d := &daemon{
		provider: p,
		idle:     10 * time.Millisecond,
		ready:    5 * time.Second,
		refresh:  time.Hour,
		units:    map[string]*unit{"ref": u},
		stop:     map[string]context.CancelFunc{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = u.serve(ctx, p, leg{
			Listen:   port,
			Upstream: provider.Endpoint{Host: "127.0.0.1", Port: up.Addr().(*net.TCPAddr).Port},
		}, 5*time.Second)
	}()

	waitForListener(t, port)

	done := make(chan struct{})

	// Reap as fast as the loop will go, while connections keep arriving.
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				d.reap(ctx)
			}
		}
	}()

	var wg sync.WaitGroup

	for range 30 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			c, err := net.DialTimeout("tcp", listenAddr(port), 5*time.Second)
			if err != nil {
				return
			}
			defer c.Close()

			_, _ = c.Write([]byte("y"))

			buf := make([]byte, 1)
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, _ = c.Read(buf)
		}()
	}

	wg.Wait()
	close(done)

	// No assertion on counts: sleeping a unit that a caller is about to use is legitimate,
	// because the next connection wakes it again. What must not happen is a data race, and
	// that is what -race is here to say.
}

func freePort(tb testing.TB) int {
	tb.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	return port
}
