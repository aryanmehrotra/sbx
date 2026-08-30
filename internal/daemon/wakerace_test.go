package daemon

import (
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A branching dependency graph must not kill the daemon.
//
// wakeAll shares one visited set across the whole recursion, and siblings are woken in
// parallel. The set was a bare map and the only mutex in wakeAll is a fresh local one per
// invocation, so two siblings that each have their OWN dependencies descend into wakeAll at
// the same moment and write that map concurrently. Go answers a concurrent map write with
// fatal(), which no recover catches: `sbx serve` dies and every sandbox on the machine loses
// its ports.
//
// It takes a graph that branches into further dependencies to reach at all. Every earlier test
// here is a chain (gateway -> config -> mysql) or a two-cycle, which is why this went unseen -
// and a fourteen-service stack is exactly the shape that has it.
func TestWakeSurvivesABranchingDependencyGraph(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &recorder{}

	// gateway ─┬─ api    ── mysql
	//          └─ worker ── redis
	// Two siblings, each with a dependency of its own: the minimum that makes two nested
	// wakeAll calls run at once.
	mysql := newUnit("zn", "mysql", "mysql", "i-mysql", "zn/mysql", nil, false)
	redis := newUnit("zn", "redis", "redis", "i-redis", "zn/redis", nil, false)
	api := newUnit("zn", "api", "api", "i-api", "zn/api", nil, false)
	worker := newUnit("zn", "worker", "worker", "i-worker", "zn/worker", nil, false)
	gw := newUnit("zn", "gateway", "gateway", "i-gateway", "zn/gateway", nil, false)

	api.dependsOn = []string{"mysql"}
	worker.dependsOn = []string{"redis"}
	gw.dependsOn = []string{"api", "worker"}

	d := &daemon{provider: p, units: map[string]*unit{
		mysql.name: mysql, redis.name: redis, api.name: api, worker.name: worker, gw.name: gw,
	}}

	for _, u := range []*unit{mysql, redis, api, worker, gw} {
		u.peers = d.peersOf
	}

	if err := gw.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	// Every service exactly once: the visited set has to dedupe across parallel branches as
	// well as within one.
	seen := map[string]int{}
	for _, ref := range p.started() {
		seen[ref]++
	}

	for _, want := range []string{"mysql", "redis", "api", "worker", "gateway"} {
		if seen[want] != 1 {
			t.Errorf("%s started %d times, want exactly 1 (started: %v)", want, seen[want], p.started())
		}
	}
}

// The same graph, repeatedly, so the race has many chances to land under -race.
func TestABranchingGraphWakesRepeatedlyWithoutRacing(t *testing.T) {
	log.SetOutput(io.Discard)

	for range 25 {
		p := &recorder{}

		a := newUnit("s", "a", "a", "i-a", "s/a", nil, false)
		b := newUnit("s", "b", "b", "i-b", "s/b", nil, false)
		c := newUnit("s", "c", "c", "i-c", "s/c", nil, false)
		dd := newUnit("s", "d", "d", "i-d", "s/d", nil, false)
		top := newUnit("s", "top", "top", "i-top", "s/top", nil, false)

		a.dependsOn = []string{"c"}
		b.dependsOn = []string{"d"}
		top.dependsOn = []string{"a", "b"}

		dm := &daemon{provider: p, units: map[string]*unit{
			a.name: a, b.name: b, c.name: c, dd.name: dd, top.name: top,
		}}

		for _, u := range []*unit{a, b, c, dd, top} {
			u.peers = dm.peersOf
		}

		if err := top.wake(context.Background(), p, 5*time.Second); err != nil {
			t.Fatalf("wake: %v", err)
		}
	}
}

// A diamond: two dependents share one dependency, woken in parallel. The shared unit must be
// started exactly once, which is the property the visited set exists for.
func TestADiamondStartsTheSharedDependencyOnce(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &recorder{}

	db := newUnit("s", "db", "db", "i-db", "s/db", nil, false)
	left := newUnit("s", "left", "left", "i-left", "s/left", nil, false)
	right := newUnit("s", "right", "right", "i-right", "s/right", nil, false)
	top := newUnit("s", "top", "top", "i-top", "s/top", nil, false)

	left.dependsOn = []string{"db"}
	right.dependsOn = []string{"db"}
	top.dependsOn = []string{"left", "right"}

	d := &daemon{provider: p, units: map[string]*unit{
		db.name: db, left.name: left, right.name: right, top.name: top,
	}}

	for _, u := range []*unit{db, left, right, top} {
		u.peers = d.peersOf
	}

	if err := top.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	starts := 0

	for _, ref := range p.started() {
		if ref == "db" {
			starts++
		}
	}

	if starts != 1 {
		t.Errorf("the shared dependency started %d times, want 1: %v", starts, p.started())
	}
}

// A unit woken only as somebody else's dependency must come up with a fresh idle clock.
//
// Nothing dials a dependency through the proxy, so no byte ever reaches its leg: without a
// touch on the way up it starts life already past the idle window, and the reaper can stop it
// inside the window where its dependent is still starting - before that dependent is awake and
// protecting it. The dependent then comes up into a sandbox whose datastore has just gone.
func TestAUnitWokenAsADependencyGetsAFreshIdleClock(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingProvider{}
	p.serving.Store(true)

	db := newUnit("s", "db", "db", "i-db", "s/db", nil, false)
	db.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

	if err := db.wakeSelf(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("wakeSelf: %v", err)
	}

	if db.idleFor() > time.Minute {
		t.Errorf("a freshly woken dependency reports %v idle, so the reaper may sleep it before "+
			"the service that needed it has finished starting", db.idleFor().Round(time.Second))
	}
}

// A literal upstream is pinned; a name is not.
//
// Caching a resolved name would be a bug rather than an optimisation - the answer to a lookup
// is allowed to change, and a cluster Service is the case where it does. Only an address that
// cannot move is kept, and everything else falls back to the dialler that looks it up.
func TestOnlyALiteralUpstreamIsResolvedOnce(t *testing.T) {
	for _, tc := range []struct {
		host   string
		pinned bool
	}{
		{"127.0.0.1", true},
		{"10.4.2.9", true},
		{"::1", true},
		{"db.sbx.svc.cluster.local", false},
		{"localhost", false},
		{"", false},
	} {
		l := leg{Listen: 20060, Upstream: provider.Endpoint{Host: tc.host, Port: 5432}}
		l.resolve()

		if got := l.addr != nil; got != tc.pinned {
			t.Errorf("upstream %q pinned=%v, want %v", tc.host, got, tc.pinned)
		}

		if l.addr != nil && l.addr.Port != 5432 {
			t.Errorf("upstream %q resolved to port %d, want 5432", tc.host, l.addr.Port)
		}
	}
}

// The dial has to reach the workload either way, so both paths are exercised against a real
// listener - the fast one, and the fallback a name takes.
func TestBothDialPathsReachTheUpstream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			_, _ = c.Write([]byte("ok"))
			_ = c.Close()
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port

	for _, tc := range []struct{ name, host string }{
		{"resolved literal", "127.0.0.1"},
		{"name, looked up per dial", "localhost"},
	} {
		l := leg{Listen: 1, Upstream: provider.Endpoint{Host: tc.host, Port: port}}
		l.resolve()

		c, err := l.dial()
		if err != nil {
			t.Errorf("%s: dial: %v", tc.name, err)

			continue
		}

		buf := make([]byte, 2)
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ok" {
			t.Errorf("%s: read %q, %v", tc.name, buf, err)
		}

		_ = c.Close()
	}
}
