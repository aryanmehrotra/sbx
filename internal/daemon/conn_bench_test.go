package daemon

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// What OPENING a connection costs, which is the number this package had no benchmark for.
//
// RoundTripDirect and RoundTripProxied both hold one connection open for the whole run, so
// they measure the relay and say nothing about the path a client pays on connect. That is the
// path that matters for what sbx is for: psql, redis-cli, a test runner and any client without
// a pool open a connection, do one thing and close it. Profiled, the dial was 27 of the 38
// allocations a connection cost and 87% of handle()'s CPU - unnoticed because the committed
// benchmarks amortise it to nothing over N round trips on one socket.
func connChurn(b *testing.B, addr string) {
	b.Helper()

	tcp, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}

	msg := []byte("PING\r\n")
	buf := make([]byte, len(msg))

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		c, err := net.DialTCP("tcp", nil, tcp)
		if err != nil {
			b.Fatal(err)
		}

		// Closing with a zero linger sends RST instead of FIN, so the socket does not sit in
		// TIME_WAIT. Without it a few thousand iterations exhaust the ephemeral port range and
		// the benchmark measures the exhaustion rather than the daemon.
		_ = c.SetLinger(0)

		if _, err := c.Write(msg); err != nil {
			b.Fatal(err)
		}

		if _, err := io.ReadFull(c, buf); err != nil {
			b.Fatal(err)
		}

		_ = c.Close()
	}
}

// BenchmarkConnDirect is the floor: connect straight to the upstream, one exchange, close.
func BenchmarkConnDirect(b *testing.B) {
	up := echoServer(b)
	connChurn(b, up.Addr().String())
}

// BenchmarkConnProxied is the same churn through a unit. The difference is what the daemon
// charges a client for showing up, and it is the figure to watch when the dial path changes.
func BenchmarkConnProxied(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })

	up := echoServer(b)

	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}

	port := front.Addr().(*net.TCPAddr).Port
	_ = front.Close() // serve() binds it itself; this was only to claim a free port

	u := newUnit("bench", "svc", "ref", "inst-ref", "bench", nil, true)
	u.mu.Lock()
	u.served = true
	u.mu.Unlock()

	upAddr := up.Addr().(*net.TCPAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lg := leg{Listen: port, Upstream: provider.Endpoint{Host: "127.0.0.1", Port: upAddr.Port}}
	lg.resolve()

	go func() { _ = u.serve(ctx, alwaysServing{}, lg, 5*time.Second) }()

	waitForListener(b, port)
	connChurn(b, net.JoinHostPort("127.0.0.1", itoa(port)))
}
