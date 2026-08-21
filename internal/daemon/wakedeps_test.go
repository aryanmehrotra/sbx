package daemon

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

// recorder notes the order services were started in, so a test can assert that a dependency
// was up before the thing that declared it.
type recorder struct {
	alwaysServing

	mu    sync.Mutex
	order []string
}

func (r *recorder) Start(_ context.Context, ref string) error {
	r.mu.Lock()
	r.order = append(r.order, ref)
	r.mu.Unlock()

	return nil
}

func (r *recorder) started() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.order...)
}

// A service that declares a dependency cannot be woken alone.
//
// sbx wakes what a connection asked for, which is right for one postgres and wrong for a
// stack: the gateway comes up, dials config, and finds a container that is stopped and
// therefore not in the network's DNS at all - `lookup config on 127.0.0.11:53: no such host`
// - and dies. Measured on a real fourteen-service sandbox before this existed.
func TestWakeStartsDependenciesFirst(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &recorder{}

	cfg := newUnit("zn", "config", "config", "i-config", "zn/config", nil, false)
	gw := newUnit("zn", "gateway", "gateway", "i-gateway", "zn/gateway", nil, false)
	gw.dependsOn = []string{"config"}

	d := &daemon{provider: p, units: map[string]*unit{cfg.name: cfg, gw.name: gw}}
	gw.peers = d.peersOf

	if err := gw.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	got := p.started()
	if len(got) != 2 {
		t.Fatalf("started %v, want config then gateway", got)
	}

	if got[0] != "config" || got[1] != "gateway" {
		t.Errorf("started %v, want [config gateway]", got)
	}
}

// A unit with no dependencies takes exactly the path it took before.
//
// This is the property that keeps the published wake numbers honest: a lone redis has nothing
// to wait for, so waking it must not acquire a second round trip on the way.
func TestWakeWithNoDependenciesStartsOnlyItself(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &recorder{}

	u := newUnit("solo", "redis", "redis", "i-redis", "solo/redis", nil, false)

	d := &daemon{provider: p, units: map[string]*unit{u.name: u}}
	u.peers = d.peersOf

	if err := u.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	if got := p.started(); len(got) != 1 || got[0] != "redis" {
		t.Errorf("started %v, want [redis] only", got)
	}
}

// Dependencies are transitive: waking the gateway must reach what config needs too.
func TestWakeFollowsTheChain(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &recorder{}

	my := newUnit("zn", "mysql", "mysql", "i-mysql", "zn/mysql", nil, false)
	cfg := newUnit("zn", "config", "config", "i-config", "zn/config", nil, false)
	cfg.dependsOn = []string{"mysql"}
	gw := newUnit("zn", "gateway", "gateway", "i-gateway", "zn/gateway", nil, false)
	gw.dependsOn = []string{"config"}

	d := &daemon{provider: p, units: map[string]*unit{
		my.name: my, cfg.name: cfg, gw.name: gw,
	}}
	for _, u := range d.units {
		u.peers = d.peersOf
	}

	if err := gw.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	got := p.started()
	if len(got) != 3 || got[0] != "mysql" || got[1] != "config" || got[2] != "gateway" {
		t.Errorf("started %v, want [mysql config gateway]", got)
	}
}

// A cycle must not hang the wake path. Nothing declares one on purpose, but a spec is written
// by hand and the daemon holding a connection open forever is a worse answer than starting
// both and moving on.
func TestWakeSurvivesADependencyCycle(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &recorder{}

	a := newUnit("zn", "a", "a", "i-a", "zn/a", nil, false)
	b := newUnit("zn", "b", "b", "i-b", "zn/b", nil, false)
	a.dependsOn = []string{"b"}
	b.dependsOn = []string{"a"}

	d := &daemon{provider: p, units: map[string]*unit{a.name: a, b.name: b}}
	a.peers, b.peers = d.peersOf, d.peersOf

	done := make(chan error, 1)
	go func() { done <- a.wake(context.Background(), p, 5*time.Second) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wake: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wake did not return: a dependency cycle hung the daemon")
	}
}

// A dependency in another sandbox is not a dependency. Names are only unique within one, so
// resolving across would wake a stranger's database because it happens to be called mysql.
func TestWakeDoesNotCrossSandboxes(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &recorder{}

	mine := newUnit("mine", "gateway", "gateway", "i-gw", "mine/gateway", nil, false)
	mine.dependsOn = []string{"mysql"}
	theirs := newUnit("theirs", "mysql", "their-mysql", "i-my", "theirs/mysql", nil, false)

	d := &daemon{provider: p, units: map[string]*unit{mine.name: mine, theirs.name: theirs}}
	mine.peers = d.peersOf

	if err := mine.wake(context.Background(), p, 5*time.Second); err != nil {
		t.Fatalf("wake: %v", err)
	}

	for _, ref := range p.started() {
		if ref == "their-mysql" {
			t.Error("woke a service in another sandbox")
		}
	}
}
