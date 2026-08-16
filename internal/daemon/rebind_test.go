package daemon

// A unit whose listener could not bind must be tried again.
//
// discover() skips anything already in d.units, and a failed serve() used to leave the unit
// sitting there - so one "address already in use" made that sandbox unreachable for the life
// of a daemon designed to run for weeks. Nothing reported an error afterwards, because
// nothing tried again.
//
// The way in is ordinary rather than exotic: `sbx rm x`, then a new sandbox moments later
// reuses the freed port slot, and its bind lands while the old listener is still closing.
// Found by a CI run of the build use case, where three sandboxes are created and removed in
// sequence and the third was never served.

import (
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// listsOneUnit is a provider that always reports the same sandbox, which is what discover()
// sees on every tick while a sandbox exists.
type listsOneUnit struct {
	countingProvider

	unit provider.Unit
}

func (l *listsOneUnit) List(context.Context, string) ([]provider.Unit, error) {
	return []provider.Unit{l.unit}, nil
}

func TestDiscoverRetriesAUnitThatCouldNotBind(t *testing.T) {
	log.SetOutput(io.Discard)

	// Hold the port the daemon wants, so its first bind cannot succeed.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := blocker.Addr().(*net.TCPAddr).Port
	up := echoServer(t)
	upAddr := up.Addr().(*net.TCPAddr)

	p := &listsOneUnit{unit: provider.Unit{
		Sandbox:  "s",
		Service:  "svc",
		Ref:      "sbx-s-svc",
		Running:  true,
		Listen:   []int{port},
		Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: upAddr.Port}},
		Client:   []provider.Endpoint{{Host: "127.0.0.1", Port: port}},
	}}
	p.serving.Store(true)

	d := &daemon{
		provider: p,
		idle:     time.Minute,
		ready:    2 * time.Second,
		units:    map[string]*unit{},
		stop:     map[string]context.CancelFunc{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First tick: the unit is registered and its listener fails, because the port is taken.
	d.discover(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for {
		d.mu.Lock()
		_, known := d.units["sbx-s-svc"]
		d.mu.Unlock()

		if !known {
			break // the failed bind was noticed and the unit dropped
		}

		if time.Now().After(deadline) {
			t.Fatal("a unit that could not bind is still registered - discover() skips it as " +
				"already known, so that sandbox stays unreachable until the daemon restarts")
		}

		time.Sleep(20 * time.Millisecond)
	}

	// Now free the port and tick again. The whole point of dropping it is that this works.
	_ = blocker.Close()

	d.discover(ctx)

	waitForListener(t, port)

	d.mu.Lock()
	_, back := d.units["sbx-s-svc"]
	d.mu.Unlock()

	if !back {
		t.Error("the unit was not rebuilt on the next tick")
	}
}

// A unit has one goroutine per leg and they can fail together. The second one to arrive must
// not delete a replacement a later tick has already built - it would take a healthy sandbox
// offline for a tick, which is the failure this whole file is about, arriving by the door the
// fix opened.
func TestForgetLeavesARebuiltUnitAlone(t *testing.T) {
	d := &daemon{
		units: map[string]*unit{},
		stop:  map[string]context.CancelFunc{},
	}

	stale := newUnit("s", "svc", "sbx-s-svc", "inst-sbx-s-svc", "sbx-s-svc", nil, true)
	fresh := newUnit("s", "svc", "sbx-s-svc", "inst-sbx-s-svc", "sbx-s-svc", nil, true)

	cancelled := false

	d.units["sbx-s-svc"] = fresh
	d.stop["sbx-s-svc"] = func() { cancelled = true }

	// The straggler from the previous generation reports its failure late.
	d.forget("sbx-s-svc", stale)

	if d.units["sbx-s-svc"] != fresh {
		t.Error("a late failure from the old unit deleted the rebuilt one")
	}

	if cancelled {
		t.Error("a late failure from the old unit cancelled the rebuilt one's listeners")
	}

	// The rebuilt unit failing on its own account still drops it.
	d.forget("sbx-s-svc", fresh)

	if _, ok := d.units["sbx-s-svc"]; ok {
		t.Error("the unit's own failure did not drop it")
	}

	if !cancelled {
		t.Error("the unit's own failure did not cancel its listeners")
	}
}
