package daemon

// The cost of a connection to a sandbox that is already awake.
//
// wake() used to Probe on every single connection. Probe shells out to `docker exec` to run
// the health command, which measured at 68 ms median per connection against 0.8 ms straight
// to docker — paid forever, not once. It made the proxy's own docstring ("the caller's first
// query pays this; nothing else ever does") false, and it made the published ~33 µs proxy
// overhead a statement about bytes on an already-open connection rather than about using the
// thing. Anything opening a connection per operation — psql, a CLI, any client without a
// pool — paid the exec every time.
//
// The fast path is optimistic, so the tests that matter are the two halves of that bet: it
// must not ask when it already knows, and it must find out when it was wrong.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// The regression this file exists for. Once a unit is awake, waking it again must cost
// nothing — no probe, no exec, no docker.
func TestAnAwakeUnitIsNotProbedAgain(t *testing.T) {
	p := &countingProvider{}
	u := newUnit("s", "svc", "ref", "s/svc", nil, false)

	if err := u.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("first wake: %v", err)
	}

	after := p.probes.Load()
	if after == 0 {
		t.Fatal("the first wake did not probe at all — this test would then prove nothing")
	}

	for range 50 {
		if err := u.wake(context.Background(), p, 5*time.Second); err != nil {
			t.Fatalf("subsequent wake: %v", err)
		}
	}

	if got := p.probes.Load(); got != after {
		t.Errorf("50 further connections cost %d extra probes; each one is a `docker exec`", got-after)
	}
}

// The other half of the bet. The daemon is the only thing that sleeps a unit, but it is not
// the only thing that can stop a container — a `docker stop`, a daemon restart, an OOM kill.
// Believing "awake" forever would leave every future connection failing.
func TestAFailedDialRevokesAwake(t *testing.T) {
	p := &countingProvider{}
	u := newUnit("s", "svc", "ref", "s/svc", nil, false)

	if err := u.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	if !u.isAwake() {
		t.Fatal("not awake after a successful wake")
	}

	// What handle() does when the upstream will not accept: revoke, then wake for real.
	p.serving.Store(false)
	u.setAwake(false)

	startsBefore := p.starts.Load()

	if err := u.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("re-wake: %v", err)
	}

	if p.starts.Load() == startsBefore {
		t.Error("the re-wake did not start the container — a unit stopped behind sbx's back stays down")
	}

	if !u.isAwake() {
		t.Error("not awake after the re-wake")
	}
}

// End to end through handle(), which is where the revoke actually lives: a client connects,
// the upstream is dead, and the connection must not hang — it either recovers or closes.
func TestHandleDoesNotHangOnADeadUpstream(t *testing.T) {
	p := &countingProvider{}
	p.serving.Store(true)

	u := newUnit("s", "svc", "ref", "s/svc", nil, true)

	// A port with nothing on it: bind one, learn its number, and let it go.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	dead := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()

	l := leg{
		Listen:   front.Addr().(*net.TCPAddr).Port,
		Upstream: provider.Endpoint{Host: "127.0.0.1", Port: dead},
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		c, err := front.Accept()
		if err != nil {
			return
		}

		u.handle(context.Background(), p, c, l, 2*time.Second)
	}()

	client, err := net.Dial("tcp", listenAddr(l.Listen))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("handle never returned against a dead upstream — a client would hang here")
	}

	// And the belief was revoked rather than kept: a second connection must probe again.
	if u.isAwake() && p.probes.Load() == 0 {
		t.Error("still believed awake with a dead upstream, and never re-checked")
	}
}
