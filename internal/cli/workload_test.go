package cli

// `sbx ready` has to prove the path to the workload, not just to the daemon.
//
// Found live on Kata in nested colima: the guest had no network, so the host's backing port was
// docker-proxy accepting and immediately closing - `redis-cli ping` got "Server closed the
// connection" - while `sbx ready` said "is serving" and exited 0, because the only dial it made
// was to the daemon's own port, which answers whether or not anything is behind it.

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// holding accepts and keeps every connection open without writing, the way redis and postgres
// wait for the client to speak first.
func holding(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu    sync.Mutex
		conns []net.Conn
	)

	t.Cleanup(func() {
		_ = ln.Close()

		mu.Lock()
		defer mu.Unlock()

		for _, c := range conns {
			_ = c.Close()
		}
	})

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port
}

// greeting accepts, writes a banner and keeps the connection open, like mysql or ssh.
func greeting(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			_, _ = io.WriteString(c, "hello\n")

			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port
}

// refused is a port that was bound and released, so nothing accepts on it.
func refused(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	return port
}

func tcpDial(port int) provider.DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", (provider.Endpoint{Host: "127.0.0.1", Port: port}).String())
	}
}

func TestProbeWorkload(t *testing.T) {
	cases := []struct {
		name    string
		port    int
		serving bool
		says    string
	}{
		{"accepts and closes, as docker-proxy does with nothing behind it", listening(t), false, "closed it"},
		{"holds the connection open, as redis does", holding(t), true, ""},
		{"speaks first, as mysql does", greeting(t), true, ""},
		{"refuses", refused(t), false, "refused"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			serving, observed := probeWorkload(context.Background(), tcpDial(c.port))
			if serving != c.serving {
				t.Fatalf("serving = %v (%s), want %v", serving, observed, c.serving)
			}

			if !strings.Contains(observed, c.says) {
				t.Errorf("observed %q, want it to say %q", observed, c.says)
			}
		})
	}
}

// upstreamStub is one healthy, running local service: the daemon's port accepts (daemon is a
// holding listener) and the upstream is whatever the test says.
type upstreamStub struct {
	provider.Provider

	daemon, upstream int
}

func (s *upstreamStub) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	return []provider.Unit{{
		Sandbox: sandbox, Service: "redis", Ref: "sbx-x-redis", Running: true,
		Client:   []provider.Endpoint{{Host: "127.0.0.1", Port: s.daemon}},
		Listen:   []int{s.daemon},
		Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: s.upstream}},
	}}, nil
}

func (s *upstreamStub) Healthy(context.Context, string) (bool, bool) { return true, true }
func (s *upstreamStub) Probe(context.Context, string) (bool, bool)   { return true, true }

func TestReadyRefusesAWorkloadThatClosesEveryConnection(t *testing.T) {
	p := &upstreamStub{daemon: holding(t), upstream: listening(t)}

	err := Ready(context.Background(), p, "x", 700*time.Millisecond)
	if err == nil {
		t.Fatal("sbx ready said serving for a workload that closes every connection")
	}

	for _, want := range []string{"redis", "closed it", "127.0.0.1:", "sbx logs x redis"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
}

func TestReadyRefusesAWorkloadNothingAcceptsFor(t *testing.T) {
	p := &upstreamStub{daemon: holding(t), upstream: refused(t)}

	err := Ready(context.Background(), p, "x", 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("want a refusal naming what was observed, got %v", err)
	}
}

func TestReadyPassesAWorkloadThatWaitsForTheClient(t *testing.T) {
	p := &upstreamStub{daemon: holding(t), upstream: holding(t)}

	if err := Ready(context.Background(), p, "x", 2*time.Second); err != nil {
		t.Fatalf("a workload that holds the connection open is serving: %v", err)
	}
}
