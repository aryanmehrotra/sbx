package daemon

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// outOfBand is a provider whose containers really start and stop: "b" answers on a TCP port only
// while running. Everything starts running; stopAll is `sbx sleep`, done behind the daemon's back.
type outOfBand struct {
	alwaysServing

	bPort int

	mu      sync.Mutex
	running map[string]bool
	starts  map[string]int
	lists   int
	ln      net.Listener
}

func (p *outOfBand) listenB() {
	ln, err := net.Listen("tcp", "127.0.0.1:"+itoa(p.bPort))
	if err != nil {
		return
	}

	p.ln = ln

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			_, _ = c.Write([]byte("b"))
			_ = c.Close()
		}
	}()
}

func (p *outOfBand) Start(_ context.Context, ref string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.starts[ref]++

	if !p.running[ref] && ref == "b" {
		p.listenB()
	}

	p.running[ref] = true

	return nil
}

func (p *outOfBand) stopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for ref := range p.running {
		p.running[ref] = false
	}

	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
}

func (p *outOfBand) Probe(_ context.Context, ref string) (bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.running[ref], true
}

func (p *outOfBand) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.lists++

	out := []provider.Unit{}
	for ref, up := range p.running {
		out = append(out, provider.Unit{Ref: ref, Sandbox: sandbox, Service: ref, Running: up})
	}

	return out, nil
}

func (p *outOfBand) state(ref string) (running bool, starts int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.running[ref], p.starts[ref]
}

// stack builds b depends_on a, both believed awake and served, with b fronted on a real port.
func stack(t *testing.T, p *outOfBand) (a, b *unit, front int) {
	t.Helper()

	front = freePort(t)

	a = newUnit("r3", "a", "a", "", "a", nil, true)
	b = newUnit("r3", "b", "b", "", "b",
		legsOf(p, provider.Unit{Listen: []int{front}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: p.bPort}}}), true)
	b.dependsOn = []string{"a"}

	peers := func(_ string, names []string) []*unit {
		var out []*unit

		for _, n := range names {
			if n == "a" {
				out = append(out, a)
			}
		}

		return out
	}
	a.peers, b.peers = peers, peers

	for _, u := range []*unit{a, b} {
		u.served = true
	}

	return a, b, front
}

func newOutOfBand(t *testing.T) *outOfBand {
	p := &outOfBand{bPort: freePort(t), running: map[string]bool{"a": true, "b": true}, starts: map[string]int{}}
	p.listenB()
	t.Cleanup(p.stopAll)

	return p
}

// `sbx sleep` stops every service through the provider, behind the daemon's back, so the daemon
// still believes both awake. The first connection to b wakes b - its failed dial revokes b's own
// belief - but its dependency a returned at wakeSelf's fast path on the stale belief, and b came
// up with a stopped: reproduced 5 of 5 on RC2. Nothing dials a, so nothing ever corrected it until
// a discovery tick, and then only for a NEW connection to b.
func TestAWakeAfterAnOutOfBandSleepStartsTheDependencyToo(t *testing.T) {
	log.SetOutput(io.Discard)

	p := newOutOfBand(t)
	_, b, front := stack(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = b.serve(ctx, p, b.legs[0], 5*time.Second) }()

	// Wait for the front to be bound. Checked with a full round trip while everything is still
	// running, so the probe itself leaves every belief true.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(front), 200*time.Millisecond); err == nil {
			_, _ = io.ReadAll(c)
			_ = c.Close()

			break
		}

		if time.Now().After(deadline) {
			t.Fatal("b's front port never came up")
		}

		time.Sleep(10 * time.Millisecond)
	}

	p.stopAll() // sbx sleep

	c, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(front), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	buf := make([]byte, 1)
	if _, err := c.Read(buf); err != nil || buf[0] != 'b' {
		t.Fatalf("the connection to b was not served after its wake: %v", err)
	}

	if up, starts := p.state("a"); !up || starts == 0 {
		t.Fatalf("b was served but its dependency a is running=%v (started %d times): "+
			"a stale belief was trusted for a dependency", up, starts)
	}
}

// A cold wake of b checks its dependencies too: a believed awake but actually stopped (a crash, a
// `docker stop`) is started, not assumed.
func TestAColdWakeStartsADependencyTheDaemonWronglyBelievesAwake(t *testing.T) {
	log.SetOutput(io.Discard)

	p := newOutOfBand(t)
	a, b, _ := stack(t, p)

	p.stopAll()
	b.setAwake(false) // b is known asleep; a is still (wrongly) believed awake

	if err := b.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	if up, _ := p.state("a"); !up || !a.isAwake() {
		t.Fatalf("b cold-woke with its dependency a still stopped (running=%v)", up)
	}
}

// The hot path stays free. A connection to a b that is awake asks the provider nothing about b
// or its dependencies: that per-connection question is the 68 ms exec the fast path was built to
// remove (see wakeSelf).
func TestAWakeOfAnAwakeUnitAsksTheProviderNothing(t *testing.T) {
	log.SetOutput(io.Discard)

	p := newOutOfBand(t)
	_, b, _ := stack(t, p)

	for range 5 {
		if err := b.wake(context.Background(), p, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	p.mu.Lock()
	lists := p.lists
	p.mu.Unlock()

	if lists != 0 {
		t.Fatalf("5 connections to an awake b asked the provider %d times, want 0", lists)
	}
}
