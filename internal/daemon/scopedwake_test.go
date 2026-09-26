package daemon_test

import (
	"context"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/cli"
	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// oneService is a one-unit engine: Start opens a real listener on the backing port, Stop closes
// it, so a wake is only "serving" if something actually started it.
type oneService struct {
	provider.Provider

	mu      sync.Mutex
	unit    provider.Unit
	backing net.Listener
	starts  int
}

func (e *oneService) Name() string { return "fake" }

func (e *oneService) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if sandbox != "" && sandbox != e.unit.Sandbox {
		return nil, nil
	}

	return []provider.Unit{e.unit}, nil
}

func (e *oneService) Start(context.Context, string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.unit.Running {
		return nil
	}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(e.unit.Upstream[0].Port)))
	if err != nil {
		return err
	}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			_ = c.Close()
		}
	}()

	e.backing, e.unit.Running = ln, true
	e.starts++

	return nil
}

func (e *oneService) Stop(context.Context, string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.backing != nil {
		_ = e.backing.Close()
		e.backing = nil
	}

	e.unit.Running = false

	return nil
}

func (e *oneService) Probe(context.Context, string) (bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.unit.Running, true
}

func (e *oneService) Healthy(ctx context.Context, ref string) (bool, bool) { return e.Probe(ctx, ref) }

func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	return p
}

// `sbx sleep` then `sbx wake` on a sandbox fronted only by a daemon started with --only: the
// sleep parks it, and the wake's knock on the public port reaches the scoped daemon, which
// starts it. Run through the CLI's own Sleep and Ready against the real daemon loop.
func TestSleepAndWakeReachAScopedDaemon(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Setenv("HOME", t.TempDir())

	pub, back := freePort(t), freePort(t)
	e := &oneService{unit: provider.Unit{
		Sandbox: "osb-a", Service: "svc", Ref: "sbx-osb-a-svc",
		Client: []provider.Endpoint{{Host: "127.0.0.1", Port: pub}}, Listen: []int{pub},
		Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: back}},
	}}

	if err := e.Start(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	scope, err := daemon.ParseScope([]string{"osb-"})
	if err != nil {
		t.Fatal(err)
	}

	d := daemon.New(e, time.Hour, 5*time.Second, 50*time.Millisecond)
	d.SetScope(scope)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx)

	for deadline := time.Now().Add(5 * time.Second); !daemon.Reachable(pub); {
		if time.Now().After(deadline) {
			t.Fatal("the scoped daemon never bound the sandbox's public port")
		}

		time.Sleep(20 * time.Millisecond)
	}

	if err := cli.Sleep(ctx, e, "osb-a"); err != nil {
		t.Fatal(err)
	}

	if serving, _ := e.Probe(ctx, ""); serving {
		t.Fatal("sbx sleep left the service running")
	}

	if err := cli.Ready(ctx, e, "osb-a", 5*time.Second); err != nil {
		t.Fatalf("sbx wake against a scoped daemon: %v", err)
	}

	e.mu.Lock()
	starts := e.starts
	e.mu.Unlock()

	// One start from setup, one from the wake the knock asked the scoped daemon for.
	if starts != 2 {
		t.Errorf("provider started %d times, want 2 (setup + the scoped daemon's wake)", starts)
	}
}
