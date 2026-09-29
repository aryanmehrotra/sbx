package daemon

// A unit replaced under the same Ref must be served as the new one.
//
// discover() keys units by Ref, and a docker Ref is the container's name - derived from the
// sandbox and service, so `sbx rm x && sbx create x` produces the identical Ref. When both
// happened between two discovery ticks, the daemon found the Ref "known", skipped it, and kept
// serving the OLD slot's ports: `sbx env` printed the new ports, nothing listened on them, and
// the old unit's idle timer went on to stop the new container by name. Seen on a laptop after
// recreating a three-service sandbox; only the one service whose rm happened to straddle a tick
// came back.

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

func unitOn(instance string, listen int, upstream net.Addr) provider.Unit {
	up := upstream.(*net.TCPAddr)

	return provider.Unit{
		Sandbox:  "s",
		Service:  "svc",
		Ref:      "sbx-s-svc",
		Instance: instance,
		Running:  true,
		Listen:   []int{listen},
		Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: up.Port}},
		Client:   []provider.Endpoint{{Host: "127.0.0.1", Port: listen}},
	}
}

func discoveringDaemon(p provider.Provider) *daemon {
	return &daemon{
		provider: p,
		idle:     time.Minute,
		ready:    2 * time.Second,
		units:    map[string]*unit{},
		stop:     map[string]context.CancelFunc{},
	}
}

func eventuallyListening(t *testing.T, port int, want bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}

		if (err == nil) == want {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("port %d listening=%v, want %v", port, err == nil, want)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func servedUnit(d *daemon) *unit {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.units["sbx-s-svc"]
}

func TestDiscoverServesAUnitRecreatedUnderTheSameRef(t *testing.T) {
	log.SetOutput(io.Discard)

	up := echoServer(t)
	oldPort, newPort := freePort(t), freePort(t)

	p := &listsOneUnit{unit: unitOn("container-1", oldPort, up.Addr())}
	p.serving.Store(true)

	d := discoveringDaemon(p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.discover(ctx)
	eventuallyListening(t, oldPort, true)

	// rm + create between two ticks: same Ref, a new container, a new slot.
	p.unit = unitOn("container-2", newPort, up.Addr())
	d.discover(ctx)

	eventuallyListening(t, newPort, true)
	eventuallyListening(t, oldPort, false)

	if u := servedUnit(d); u == nil || u.instance != "container-2" {
		t.Fatalf("served unit is %+v, want the new instance", u)
	}
}

// A provider with no instance identity (kubernetes reports none) is still caught when the
// ports it is fronted on move.
func TestDiscoverRebindsAUnitWhosePortsMoved(t *testing.T) {
	log.SetOutput(io.Discard)

	up := echoServer(t)
	oldPort, newPort := freePort(t), freePort(t)

	p := &listsOneUnit{unit: unitOn("", oldPort, up.Addr())}
	p.serving.Store(true)

	d := discoveringDaemon(p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.discover(ctx)
	eventuallyListening(t, oldPort, true)

	p.unit = unitOn("", newPort, up.Addr())
	d.discover(ctx)

	eventuallyListening(t, newPort, true)
	eventuallyListening(t, oldPort, false)
}

// An unchanged unit is left alone: the same listener, the same state, tick after tick. Rebuilding
// it would drop its live connections and forget whether it was awake.
func TestDiscoverKeepsAnUnchangedUnit(t *testing.T) {
	log.SetOutput(io.Discard)

	up := echoServer(t)
	port := freePort(t)

	p := &listsOneUnit{unit: unitOn("container-1", port, up.Addr())}
	p.serving.Store(true)

	d := discoveringDaemon(p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.discover(ctx)
	eventuallyListening(t, port, true)

	first := servedUnit(d)

	d.discover(ctx)
	d.discover(ctx)

	if servedUnit(d) != first {
		t.Fatal("an unchanged unit was rebuilt by a discovery tick")
	}

	eventuallyListening(t, port, true)
}

// The common shape: the freed slot is reused, so the ports are the same and only the container
// is new. The new instance is what is served, not a unit still pointing at the old one.
func TestDiscoverServesAUnitRecreatedOnTheSamePorts(t *testing.T) {
	log.SetOutput(io.Discard)

	up := echoServer(t)
	port := freePort(t)

	p := &listsOneUnit{unit: unitOn("container-1", port, up.Addr())}
	p.serving.Store(true)

	d := discoveringDaemon(p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.discover(ctx)
	eventuallyListening(t, port, true)

	p.unit = unitOn("container-2", port, up.Addr())

	// The old listener may still be closing when the new one binds; a later tick retries it,
	// as it does for any unit that could not bind.
	deadline := time.Now().Add(5 * time.Second)

	for {
		d.discover(ctx)

		if u := servedUnit(d); u != nil && u.instance == "container-2" && listening(port) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("still serving %+v, want the new instance", servedUnit(d))
		}

		time.Sleep(50 * time.Millisecond)
	}
}
