package daemon

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A per-service "idle" shorter than the daemon's own window must be honoured at its own
// resolution. The reap ticker followed --idle alone, so under the default 5m it ticked every 30s:
// a service set to "30s" slept after 59s and one set to "20s" after ~55s - the setting could only
// ever make a service sleep on the daemon's clock, never on its own.
func TestPerServiceIdleSetsTheReapCadence(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &pausing{listingProvider: listingProvider{units: []provider.Unit{
		{Ref: "sbx-fx-api", Sandbox: "fx", Service: "api", Running: true, Idle: "1500ms",
			Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
	}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The daemon-wide window is an hour: only the service's own 1.5s can make it sleep in time.
	d := New(p, time.Hour, time.Second, time.Hour)
	go d.Run(ctx)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.seen(), "stop sbx-fx-api") {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("a service with idle 1500ms was not slept within 8s under a 1h daemon window (calls: %s)", p.seen())
}
