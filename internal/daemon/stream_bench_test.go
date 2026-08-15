package daemon

// Throughput, which nothing here measured.
//
// The published proxy tax — ~15-33 µs — is a latency figure on a six-byte PING. Every
// benchmark and script in this repo moves six bytes. The workloads sbx actually fronts are
// databases and headless browsers: a pg_dump, a COPY, a large result set, a CDP screenshot.
// Whether sitting in that path costs 5% or 300% was simply unknown, which is the position
// this project was in about the 68 ms per-connection cost before somebody measured it.
//
//	go test -run '^$' -bench Stream -benchtime 20x -count 6 ./internal/daemon | tee new.txt
//	benchstat new.txt

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

const streamBytes = 16 << 20 // 16 MiB per iteration

// sink accepts connections, reads a request byte, and writes streamBytes back.
func sink(tb testing.TB) net.Listener {
	tb.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() { _ = ln.Close() })

	payload := make([]byte, streamBytes)

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer c.Close()

				one := make([]byte, 1)
				for {
					if _, err := io.ReadFull(c, one); err != nil {
						return
					}

					if _, err := c.Write(payload); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	return ln
}

func drain(b *testing.B, addr string) {
	b.Helper()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()

	buf := make([]byte, 1<<20)

	b.SetBytes(streamBytes)
	b.ResetTimer()

	for range b.N {
		if _, err := c.Write([]byte{1}); err != nil {
			b.Fatal(err)
		}

		read := 0
		for read < streamBytes {
			n, err := c.Read(buf)
			if err != nil {
				b.Fatal(err)
			}

			read += n
		}
	}

	b.StopTimer()
}

// BenchmarkStreamDirect is the floor: the same bytes, straight from the upstream.
func BenchmarkStreamDirect(b *testing.B) {
	up := sink(b)
	drain(b, up.Addr().String())
}

// BenchmarkStreamProxied is the same bytes through a unit. The difference is what a bulk
// transfer pays for sbx being in the path.
func BenchmarkStreamProxied(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })

	up := sink(b)

	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}

	port := front.Addr().(*net.TCPAddr).Port
	_ = front.Close()

	u := newUnit("bench", "svc", "ref", "bench", nil, true)
	u.mu.Lock()
	u.served = true
	u.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = u.serve(ctx, alwaysServing{}, leg{
			Listen:   port,
			Upstream: provider.Endpoint{Host: "127.0.0.1", Port: up.Addr().(*net.TCPAddr).Port},
		}, 5*time.Second)
	}()

	waitForListener(b, port)

	drain(b, listenAddr(port))
}
