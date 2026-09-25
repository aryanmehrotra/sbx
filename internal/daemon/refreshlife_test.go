package daemon

import (
	"context"
	"io"
	"log"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A discovery pass asked for by a request - the OpenSandbox API's Refresh right after it
// creates a sandbox - must not tie the listeners it opens to that request. They belong to the
// daemon. Tied to the caller's context, the wake port closed the moment provisioning finished:
// the sandbox reported Running, its endpoint refused connections, and the unit stayed
// registered with a dead listener, so no later tick ever rebound it. That was half of
// TestDockerOpenSandboxLifecycle failing on an isolated engine ("connection refused" on thaw).
func TestAListenerOutlivesTheRequestThatDiscoveredIt(t *testing.T) {
	log.SetOutput(io.Discard)

	port := freePort(t)
	p := &listingProvider{units: []provider.Unit{{
		Ref: "sbx-osb-aaaaaaaaaaaa-sandbox", Sandbox: "osb-aaaaaaaaaaaa", Service: "sandbox", Running: true,
		Listen: []int{port}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}},
	}}}

	daemonCtx, stop := context.WithCancel(context.Background())
	defer stop()

	d := New(p, time.Hour, time.Second, time.Hour)
	go d.run(daemonCtx)

	// The run loop's own first pass may adopt the unit; wait until it has, so that the pass
	// below is the one that matters, then drop the unit so the request's pass adopts it anew.
	waitDial(t, port, true)
	d.mu.Lock()
	for ref := range d.units {
		d.stop[ref]()
		delete(d.units, ref)
		delete(d.stop, ref)
	}
	d.mu.Unlock()
	waitDial(t, port, false)

	req, done := context.WithCancel(context.Background())
	d.Refresh(req)
	waitDial(t, port, true)
	done() // the request that asked for the pass is over

	time.Sleep(200 * time.Millisecond)

	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); err != nil {
		t.Fatalf("the wake port closed when the request that discovered it ended: %v", err)
	} else {
		_ = c.Close()
	}
}

func waitDial(t *testing.T, port int, want bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)

	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}

		if (err == nil) == want {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("port %d reachable=%v, want %v", port, err == nil, want)
		}

		time.Sleep(20 * time.Millisecond)
	}
}
