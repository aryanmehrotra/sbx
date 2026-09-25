package daemon

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// vsockish is a provider whose workload is reachable only through the dialer it hands out, the
// way a Firecracker microVM is: its Upstream is a guest port no TCP dial could reach.
type vsockish struct {
	alwaysServing

	guestPort int
	dials     atomic.Int32
	asked     atomic.Int32
	guest     net.Listener // what the dialer really reaches, standing in for the guest
}

func (v *vsockish) GuestDialer(sandbox, service string, guestPort int) (provider.DialFunc, bool) {
	v.asked.Add(1)

	if sandbox != "sb" || service != "execd" || guestPort != v.guestPort {
		return nil, false
	}

	return func(ctx context.Context) (net.Conn, error) {
		v.dials.Add(1)

		var d net.Dialer

		return d.DialContext(ctx, "tcp", v.guest.Addr().String())
	}, true
}

func echoListener(t *testing.T) net.Listener {
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

			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()

	return ln
}

func guestRoundTrip(t *testing.T, port int) {
	t.Helper()

	waitForListener(t, port)

	c, err := net.Dial("tcp", listenAddr(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.WriteString(c, "hello\n"); err != nil {
		t.Fatal(err)
	}

	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("round trip through the proxy: %q %v", line, err)
	}
}

// A provider that offers a dialer gets every connection through it, not through a TCP dial of
// Upstream - which here is a port nothing listens on, so a TCP dial would fail the round trip.
func TestProxyUsesProviderDialer(t *testing.T) {
	p := &vsockish{guestPort: 44772, guest: echoListener(t)}

	legs := legsOf(p, provider.Unit{
		Sandbox:  "sb",
		Service:  "execd",
		Listen:   []int{freePort(t)},
		Upstream: []provider.Endpoint{{Host: "vsock", Port: 44772}},
	})

	if len(legs) != 1 || legs[0].dialer == nil {
		t.Fatalf("legs %+v: the provider's dialer was not attached", legs)
	}

	u := newUnit("sb", "execd", "ref", "inst", "sb-execd", legs, true)
	u.mu.Lock()
	u.served = true
	u.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = u.serve(ctx, p, legs[0], 5*time.Second) }()

	guestRoundTrip(t, legs[0].Listen)
	guestRoundTrip(t, legs[0].Listen)

	// At least one per round trip; the readiness probe in guestRoundTrip is a connection too, so
	// the count is a floor. That the round trips succeeded at all is the real proof: Upstream
	// "vsock:44772" is not an address a TCP dial can reach.
	if n := p.dials.Load(); n < 2 {
		t.Fatalf("provider dialer used %d times for 2 round trips", n)
	}
}

// ok=false from the provider, or no GuestDialer at all, is the TCP path exactly as before:
// the literal is resolved once and no dialer is attached.
func TestProxyFallsBackToTCP(t *testing.T) {
	up := echoListener(t)
	upPort := up.Addr().(*net.TCPAddr).Port

	for name, p := range map[string]provider.Provider{
		"no capability":        alwaysServing{},
		"capability, declined": &vsockish{guestPort: 1},
	} {
		t.Run(name, func(t *testing.T) {
			legs := legsOf(p, provider.Unit{
				Sandbox:  "sb",
				Service:  "execd",
				Listen:   []int{freePort(t)},
				Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: upPort}},
			})

			if legs[0].dialer != nil || legs[0].addr == nil {
				t.Fatalf("leg %+v: want the resolved TCP path and no dialer", legs[0])
			}

			u := newUnit("sb", "execd", "ref", "inst", "sb-execd", legs, true)
			u.mu.Lock()
			u.served = true
			u.mu.Unlock()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			go func() { _ = u.serve(ctx, p, legs[0], 5*time.Second) }()

			guestRoundTrip(t, legs[0].Listen)
		})
	}
}

// A provider dialer that never answers is bounded: handle() must reach its revoke-and-rewake
// path rather than hold the client and two goroutines on a VM that is not there.
func TestProviderDialerIsBounded(t *testing.T) {
	defer func(d time.Duration) { guestDialTimeout = d }(guestDialTimeout)

	guestDialTimeout = 100 * time.Millisecond

	l := leg{dialer: func(ctx context.Context) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}

	start := time.Now()

	if _, err := l.dial(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the dial cut at its deadline", err)
	}

	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("dial returned after %v", el)
	}
}
