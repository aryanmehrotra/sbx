package daemon

// The cost of a connection to a sandbox that is already awake.
//
// wake() used to Probe on every single connection. Probe shells out to `docker exec` to run
// the health command, which measured at 68 ms median per connection against 0.8 ms straight
// to docker - paid forever, not once. It made the proxy's own docstring ("the caller's first
// query pays this; nothing else ever does") false, and it made the published ~33 µs proxy
// overhead a statement about bytes on an already-open connection rather than about using the
// thing. Anything opening a connection per operation - psql, a CLI, any client without a
// pool - paid the exec every time.
//
// The fast path is optimistic, so the tests that matter are the two halves of that bet: it
// must not ask when it already knows, and it must find out when it was wrong.

import (
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// The regression this file exists for. Once a unit is awake, waking it again must cost
// nothing - no probe, no exec, no docker.
func TestAnAwakeUnitIsNotProbedAgain(t *testing.T) {
	p := &countingProvider{}
	u := newUnit("s", "svc", "ref", "inst-ref", "s/svc", nil, false)

	if err := u.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("first wake: %v", err)
	}

	after := p.probes.Load()
	if after == 0 {
		t.Fatal("the first wake did not probe at all - this test would then prove nothing")
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
// the only thing that can stop a container - a `docker stop`, a daemon restart, an OOM kill.
// Believing "awake" forever would leave every future connection failing.
func TestAFailedDialRevokesAwake(t *testing.T) {
	p := &countingProvider{}
	u := newUnit("s", "svc", "ref", "inst-ref", "s/svc", nil, false)

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
		t.Error("the re-wake did not start the container - a unit stopped behind sbx's back stays down")
	}

	if !u.isAwake() {
		t.Error("not awake after the re-wake")
	}
}

// End to end through handle(), which is where the revoke actually lives: a client connects,
// the upstream is dead, and the connection must not hang - it either recovers or closes.
func TestHandleDoesNotHangOnADeadUpstream(t *testing.T) {
	p := &countingProvider{}
	p.serving.Store(true)

	u := newUnit("s", "svc", "ref", "inst-ref", "s/svc", nil, true)

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
		t.Fatal("handle never returned against a dead upstream - a client would hang here")
	}

	// And the belief was revoked rather than kept: a second connection must probe again.
	if u.isAwake() && p.probes.Load() == 0 {
		t.Error("still believed awake with a dead upstream, and never re-checked")
	}
}

// slowStopper holds a sleep open long enough for a racing connection to be observed.
type slowStopper struct {
	countingProvider

	stopping chan struct{} // closed once Stop has begun
	release  chan struct{} // Stop returns when this is closed
}

func (s *slowStopper) Stop(_ context.Context, _ string) error {
	close(s.stopping)
	<-s.release
	s.stops.Add(1)
	s.serving.Store(false)

	return nil
}

// A connection that arrives while a sleep is committed must not be told the unit is awake.
//
// The first version of the awake fast path checked `isAwake()` BEFORE taking `u.waking`,
// which made it cheap and wrong: sleep() holds that lock across `p.Stop()`, so a connection
// could see "awake", skip straight to dialling, and reach a container that was already being
// stopped. The client got a connection reset for a sandbox it had just woken - reached twice
// in one run of the concurrent-wake suite on a busy machine.
//
// Only the probe was ever expensive. The lock costs nanoseconds, and taking it is what makes
// "awake" mean anything.
func TestWakeWaitsForACommittedSleep(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &slowStopper{stopping: make(chan struct{}), release: make(chan struct{})}
	p.serving.Store(true)

	u := newUnit("s", "svc", "ref", "inst-ref", "s/svc", nil, true)
	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

	go u.sleep(context.Background(), p, time.Minute)

	<-p.stopping // the sleep is now past the point of no return, holding u.waking

	woke := make(chan error, 1)

	go func() {
		// Fresh activity, exactly as handle() records it before waking.
		u.touch()
		woke <- u.wake(context.Background(), p, 10*time.Second)
	}()

	select {
	case <-woke:
		t.Fatal("wake returned while a sleep was mid-Stop - it did not take the lock, so a " +
			"caller would dial a container that is being stopped")
	case <-time.After(200 * time.Millisecond):
		// Correct: blocked behind the sleep.
	}

	close(p.release)

	select {
	case err := <-woke:
		if err != nil {
			t.Fatalf("wake after the sleep finished: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("wake never completed after the sleep released the lock")
	}

	if !u.isAwake() {
		t.Error("not awake after waking behind a sleep")
	}

	if p.starts.Load() == 0 {
		t.Error("the unit was reported awake without ever being started - the stale 'awake' " +
			"survived the sleep that cleared it")
	}
}

// The structural assertion behind the test above: wake() decides nothing until it holds
// u.waking, even when the unit is already believed awake.
//
// This is stated directly because the racing version cannot reach the window on demand -
// sleep() clears `awake` before it calls Stop, so a racing wake almost always sees the
// cleared flag and blocks anyway. The gap is between sleep's idle re-check and that clear,
// and it is real but too narrow to hit deterministically from a test.
//
// So: hold the lock, as sleep() does through its critical section, and require that a wake
// on an awake unit waits rather than answering from the flag alone. An implementation that
// checks isAwake() before locking returns immediately here and fails.
func TestWakeTakesTheLockEvenWhenAwake(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingProvider{}
	p.serving.Store(true)

	u := newUnit("s", "svc", "ref", "inst-ref", "s/svc", nil, true)

	u.waking.Lock()

	woke := make(chan error, 1)
	go func() { woke <- u.wake(context.Background(), p, 5*time.Second) }()

	select {
	case <-woke:
		u.waking.Unlock()
		t.Fatal("wake answered from the awake flag without taking u.waking - sleep() holds " +
			"that lock across p.Stop(), so this caller would dial a container being stopped")
	case <-time.After(150 * time.Millisecond):
	}

	u.waking.Unlock()

	select {
	case err := <-woke:
		if err != nil {
			t.Fatalf("wake: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wake never returned after the lock was released")
	}

	// And it still cost no probe: the lock is the fix, not a return to asking docker.
	if got := p.probes.Load(); got != 0 {
		t.Errorf("an awake unit was probed %d times - the 68 ms per connection is back", got)
	}
}

// A client that half-closes must still get its whole response.
//
// handle() waited on one of two pipe goroutines, so whichever direction finished first tore
// down both - undoing the CloseWrite that pipe() performs one line earlier. A client that
// signals end-of-input with shutdown(SHUT_WR) and then reads (`nc -N`, several HTTP clients,
// pipe-mode bulk loaders) got a truncated response and no error.
func TestAHalfClosedClientStillGetsTheWholeResponse(t *testing.T) {
	log.SetOutput(io.Discard)

	// An upstream that reads to EOF, then writes a reply larger than one pipe buffer.
	const replySize = 256 * 1024

	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()

	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()

		_, _ = io.Copy(io.Discard, c) // read until the client half-closes
		_, _ = c.Write(make([]byte, replySize))

		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()

	p := &countingProvider{}
	p.serving.Store(true)

	u := newUnit("s", "svc", "ref", "inst-ref", "s/svc", nil, true)

	l := leg{
		Listen:   front.Addr().(*net.TCPAddr).Port,
		Upstream: provider.Endpoint{Host: "127.0.0.1", Port: up.Addr().(*net.TCPAddr).Port},
	}

	go func() {
		c, err := front.Accept()
		if err != nil {
			return
		}

		u.handle(context.Background(), p, c, l, 5*time.Second)
	}()

	client, err := net.Dial("tcp", listenAddr(l.Listen))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}

	// "That is all I am sending" - and now the whole reply must come back.
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))

	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}

	if len(got) != replySize {
		t.Errorf("got %d bytes of a %d byte response - the response was truncated when the "+
			"client half-closed", len(got), replySize)
	}
}
