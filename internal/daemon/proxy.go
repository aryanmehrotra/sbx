package daemon

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// unit is one wakeable service and the ports that front it.
//
// One service, not one sandbox: a worktree's MySQL and its ClickHouse sleep and wake
// independently, so the analytics store a branch never queries costs nothing at all.
//
// Nothing here knows what a container is. It binds a port, asks a Provider to start
// something, waits for that thing to say it is serving, and moves bytes — which is the same
// job on a laptop and in a cluster, and the reason the activator is this file plus a
// different Provider rather than a second program.
type unit struct {
	sandbox string
	service string // for logs: which part of the sandbox this is
	ref     string // provider handle: container id, or deployment name
	name    string // for logs
	legs    []leg

	// lastByte is the only activity signal. Connection counting is not enough: a Go
	// service's pool holds idle connections open indefinitely, so a unit fronted by a
	// running service would never sleep. Bytes are what "in use" actually means.
	lastByte atomic.Int64

	mu     sync.Mutex
	live   map[net.Conn]struct{} // open client conns, closed on sleep
	awake  bool
	waking sync.Mutex // serialises wake so a burst of connections starts the workload once

	// served records that this unit has been seen serving at least once.
	//
	// Until then it is not eligible to sleep, because "idle" is meaningless before a
	// service has ever been up: a sandbox that is still pulling an image and running its
	// schema migrations looks exactly like one nobody has touched. Measured from discovery
	// instead, a 39-second creation was put to sleep underneath the command creating it.
	served bool
}

// leg is one port this daemon listens on and where it forwards.
type leg struct {
	Listen   int               // bound locally by the daemon
	Upstream provider.Endpoint // dialled once the workload is serving
}

func newUnit(sandbox, service, ref, name string, legs []leg, running bool) *unit {
	u := &unit{
		sandbox: sandbox,
		service: service,
		ref:     ref,
		name:    name,
		legs:    legs,
		live:    map[net.Conn]struct{}{},
		awake:   running,
	}
	u.touch()

	return u
}

func (u *unit) touch() { u.lastByte.Store(time.Now().UnixNano()) }

// sleepable reports whether the idle clock should count for this unit yet.
//
// The first time it is seen serving, the clock is restarted from that moment: a unit that
// took two minutes to come up has not been idle for two minutes, it has been starting.
func (u *unit) sleepable(ctx context.Context, p provider.Provider) bool {
	u.mu.Lock()
	served := u.served
	u.mu.Unlock()

	if served {
		return true
	}

	if serving, declared := p.Healthy(ctx, u.ref); !serving && declared {
		return false
	}

	u.mu.Lock()
	u.served = true
	u.mu.Unlock()
	u.touch()

	return false
}

func (u *unit) idleFor() time.Duration {
	return time.Since(time.Unix(0, u.lastByte.Load()))
}

// serve accepts on one port for the lifetime of ctx.
func (u *unit) serve(ctx context.Context, p provider.Provider, l leg, readyTimeout time.Duration) error {
	ln, err := net.Listen("tcp", listenAddr(l.Listen))
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	logs.Default.Info(u.sandbox, u.service, "listening on :%d, forwards to %s", l.Listen, l.Upstream)

	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}

		go u.handle(ctx, p, c, l, readyTimeout)
	}
}

func (u *unit) handle(ctx context.Context, p provider.Provider, client net.Conn, l leg, readyTimeout time.Duration) {
	defer client.Close()

	u.touch()

	if err := u.wake(ctx, p, readyTimeout); err != nil {
		// Hanging up is the honest failure. A caller that gets a closed connection retries
		// or reports; one that gets a silently empty stream does neither.
		logs.Default.Event(logs.LevelError, u.sandbox, u.service, "wakeFailed", 0, "could not wake: %v", err)
		return
	}

	upstream, err := net.DialTimeout("tcp", l.Upstream.String(), 10*time.Second)
	if err != nil {
		logs.Default.Error(u.sandbox, u.service, "awake but not reachable at %s: %v", l.Upstream, err)
		return
	}
	defer upstream.Close()

	u.track(client)
	defer u.untrack(client)

	done := make(chan struct{}, 2)
	go u.pipe(upstream, client, done)
	go u.pipe(client, upstream, done)
	<-done
}

// pipe copies one direction, stamping activity as it goes.
func (u *unit) pipe(dst, src net.Conn, done chan<- struct{}) {
	buf := make([]byte, 32*1024)

	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			u.touch()

			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}

		if rerr != nil {
			break
		}
	}

	// Half-close so the peer sees EOF instead of waiting on a connection nobody will write
	// to again.
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}

	done <- struct{}{}
}

// wake starts the workload if needed and blocks until it reports serving. The caller's
// first query pays this; nothing else ever does.
func (u *unit) wake(ctx context.Context, p provider.Provider, readyTimeout time.Duration) error {
	u.waking.Lock()
	defer u.waking.Unlock()

	if serving, declared := p.Probe(ctx, u.ref); serving && declared {
		u.setAwake(true)
		return nil
	}

	start := time.Now()

	if err := p.Start(ctx, u.ref); err != nil {
		return err
	}

	deadline := time.Now().Add(readyTimeout)

	for time.Now().Before(deadline) {
		serving, declared := p.Probe(ctx, u.ref)

		// Without a declared health check there is nothing to ask, and dialling the port
		// is not an answer: a published port answers before the server behind it does.
		// Give the workload a moment and go, rather than pretend to have checked.
		if !declared {
			time.Sleep(2 * time.Second)
			u.setAwake(true)
			logs.Default.Event(logs.LevelWarn, u.sandbox, u.service, "woke",
				time.Since(start).Milliseconds(),
				"woke in %dms, unverified — no health check declared",
				time.Since(start).Milliseconds())

			return nil
		}

		if serving {
			u.setAwake(true)

			u.mu.Lock()
			u.served = true
			u.mu.Unlock()

			logs.Default.Event(logs.LevelInfo, u.sandbox, u.service, "woke",
				time.Since(start).Milliseconds(), "woke in %dms", time.Since(start).Milliseconds())

			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return errNotReady{u.name, readyTimeout}
}

// sleep stops the workload and closes anything still attached to it. Closing the live
// connections is not rudeness: the client reconnects, the reconnect wakes the unit, and the
// alternative is a pool holding a sandbox up for a branch nobody is working on.
// sleep stops the unit. It takes the same lock wake() does, because the two drive one
// container in opposite directions and the interleaving is visible to a client.
//
// Without the lock a connection arriving mid-stop found Probe still reporting the old
// state, was told the sandbox was awake, and was handed a connection to a container that
// was in the process of stopping. The daemon then recorded it asleep, so the daemon and
// the caller disagreed about the same sandbox.
//
// idle is re-checked here rather than trusted from the reaper: between the reaper deciding
// and this acquiring the lock a client may have arrived and touched the unit, and stopping
// then is stopping something in use.
func (u *unit) sleep(ctx context.Context, p provider.Provider, idle time.Duration) {
	u.waking.Lock()
	defer u.waking.Unlock()

	if u.idleFor() < idle {
		return
	}

	u.mu.Lock()
	for c := range u.live {
		_ = c.Close()
	}

	u.live = map[net.Conn]struct{}{}
	u.mu.Unlock()

	// Marked asleep before the stop returns, so anything blocked on the lock behind this
	// takes the wake path instead of believing a stale "awake".
	u.setAwake(false)

	if err := p.Stop(ctx, u.ref); err != nil {
		logs.Default.Error(u.sandbox, u.service, "could not sleep: %v", err)
		return
	}
	logs.Default.Event(logs.LevelInfo, u.sandbox, u.service, "slept",
		u.idleFor().Milliseconds(), "slept — idle for %s", u.idleFor().Round(time.Second))
}

func (u *unit) track(c net.Conn) {
	u.mu.Lock()
	u.live[c] = struct{}{}
	u.mu.Unlock()
}

func (u *unit) untrack(c net.Conn) {
	u.mu.Lock()
	delete(u.live, c)
	u.mu.Unlock()
	u.touch()
}

func (u *unit) isAwake() bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.awake
}

func (u *unit) setAwake(v bool) {
	u.mu.Lock()
	u.awake = v
	u.mu.Unlock()
}

// listenAddr binds loopback on a laptop and every interface in a pod, because there the
// connection arrives from a Service on the pod network rather than from this machine.
func listenAddr(port int) string {
	if InCluster() {
		return "0.0.0.0:" + itoa(port)
	}

	return "127.0.0.1:" + itoa(port)
}
