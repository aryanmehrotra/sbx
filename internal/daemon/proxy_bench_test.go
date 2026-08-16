package daemon

// What the proxy costs once a sandbox is awake.
//
// The wake is a one-off a human notices; this is the tax on every query for the life of the
// sandbox, which is the number that decides whether sitting in the data path is acceptable
// at all. Run it against itself:
//
//	go test -run '^$' -bench RoundTrip -count 10 | tee new.txt
//	benchstat new.txt
//
// -count matters. A single run of either of these is dominated by whatever else the machine
// was doing, and the difference being measured is tens of microseconds.

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
)

// alwaysServing is a Provider that says yes. The benchmark is measuring the splice, not the
// wake, so the wake must not be in the loop.
type alwaysServing struct{}

func (alwaysServing) Name() string { return "bench" }
func (alwaysServing) Create(context.Context, string, int, int, string, spec.Service, []provider.Endpoint, string, provider.Isolation) error {
	return nil
}
func (alwaysServing) Start(context.Context, string) error                   { return nil }
func (alwaysServing) Stop(context.Context, string) error                    { return nil }
func (alwaysServing) Healthy(context.Context, string) (bool, bool)          { return true, true }
func (alwaysServing) Probe(context.Context, string) (bool, bool)            { return true, true }
func (alwaysServing) List(context.Context, string) ([]provider.Unit, error) { return nil, nil }
func (alwaysServing) Remove(context.Context, string) error                  { return nil }
func (alwaysServing) Commit(context.Context, string, string) error          { return nil }

func (alwaysServing) Images(context.Context, string) ([]string, error) { return nil, nil }

func (alwaysServing) CopyVolume(context.Context, string, string) error { return nil }

func (alwaysServing) VolumeFor(string, string) string { return "" }

func (alwaysServing) ExecTTY(context.Context, string, []string) error { return nil }

func (alwaysServing) Exec(context.Context, string, []string) (string, error)   { return "", nil }
func (alwaysServing) Logs(context.Context, string, int, bool, io.Writer) error { return nil }
func (alwaysServing) Copy(context.Context, string, string, string) error       { return nil }
func (alwaysServing) Endpoints(string, string, int, int, []int) []provider.Endpoint {
	return nil
}
func (alwaysServing) AllocSlot(context.Context, string) (int, error) { return 0, nil }

// echoServer is the upstream: it returns what it is sent, so a round trip measures transport
// and nothing else.
func echoServer(tb testing.TB) net.Listener {
	tb.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	tb.Cleanup(func() { _ = ln.Close() })

	return ln
}

func roundTrips(b *testing.B, addr string) {
	b.Helper()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()

	_ = c.(*net.TCPConn).SetNoDelay(true)

	msg := []byte("PING\r\n")
	buf := make([]byte, len(msg))

	// One exchange outside the timer so a connection setup and a cold path do not land in
	// the first sample.
	if _, err := c.Write(msg); err != nil {
		b.Fatal(err)
	}

	if _, err := io.ReadFull(c, buf); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for b.Loop() {
		if _, err := c.Write(msg); err != nil {
			b.Fatal(err)
		}

		if _, err := io.ReadFull(c, buf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRoundTripDirect is the floor: a client talking straight to the upstream.
// The daemon logs a line per listener. Attached to a benchmark those land in the same
// stream as the results, and benchstat drops every row it cannot parse - which is how the
// proxied row silently ended up with n=1 while the direct row had six. None of these
// benchmarks are measuring the logger.
func init() { logs.Default = logs.New(io.Discard) }

func BenchmarkRoundTripDirect(b *testing.B) {
	up := echoServer(b)
	roundTrips(b, up.Addr().String())
}

// BenchmarkRoundTripProxied is the same exchange through a unit. The difference between the
// two is the number that matters.
func BenchmarkRoundTripProxied(b *testing.B) {
	// The daemon narrates what it is fronting, which is right in operation and noise in a
	// benchmark: the line lands in the middle of the samples and in benchstat's input.
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })

	up := echoServer(b)

	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}

	port := front.Addr().(*net.TCPAddr).Port
	_ = front.Close() // serve() binds it itself; this was only to claim a free port

	u := newUnit("bench", "svc", "ref", "bench", nil, true)
	u.mu.Lock()
	u.served = true
	u.mu.Unlock()

	upAddr := up.Addr().(*net.TCPAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = u.serve(ctx, alwaysServing{}, leg{
			Listen:   port,
			Upstream: provider.Endpoint{Host: "127.0.0.1", Port: upAddr.Port},
		}, 5*time.Second)
	}()

	waitForListener(b, port)

	roundTrips(b, net.JoinHostPort("127.0.0.1", itoa(port)))
}

func waitForListener(tb testing.TB, port int) {
	tb.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	tb.Fatalf("proxy never started listening on :%d", port)
}
