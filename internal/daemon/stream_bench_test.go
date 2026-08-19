package daemon

// Throughput, which nothing here measured.
//
// The published proxy tax - ~15-33 µs - is a latency figure on a six-byte PING. Every
// benchmark and script in this repo moves six bytes. The workloads sbx actually fronts are
// databases and headless browsers: a pg_dump, a COPY, a large result set, a CDP screenshot.
// Whether sitting in that path costs 5% or 300% was simply unknown, which is the position
// this project was in about the 68 ms per-connection cost before somebody measured it.
//
//	go test -run '^$' -bench Stream -benchtime 20x -count 6 ./internal/daemon | tee new.txt
//	benchstat new.txt

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
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

	u := newUnit("bench", "svc", "ref", "inst-ref", "bench", nil, true)
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

// BenchmarkStreamBuf sweeps the relay buffer size in one invocation, so the comparison is
// robust to a machine whose load drifts between separate runs - the failure mode CONTRIBUTING.md
// warns about. Run: go test -run '^$' -bench StreamBuf -count 8 ./internal/daemon/
func BenchmarkStreamBuf(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })

	saved := relayBuf
	b.Cleanup(func() {
		relayBuf = saved
		relayBufPool = sync.Pool{New: func() any { x := make([]byte, relayBuf); return &x }}
	})

	for _, size := range []int{32 << 10, 64 << 10, 128 << 10, 256 << 10} {
		size := size
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			relayBuf = size
			relayBufPool = sync.Pool{New: func() any { x := make([]byte, relayBuf); return &x }}

			up := sink(b)

			front, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			port := front.Addr().(*net.TCPAddr).Port
			_ = front.Close()

			u := newUnit("bench", "svc", "ref", "inst-ref", "bench", nil, true)
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
		})
	}
}

// BenchmarkRelayBufAcquire isolates the allocation cost of the copy buffer from all network and
// scheduler noise - it is pure CPU, so it is robust on a busy machine where the throughput
// benchmarks are not. It proves the one load-independent claim: pooling removes the per-connection
// 128 KiB allocation the old code paid on every tunnel. Two directions per connection, so the
// real saving is twice what one iteration here shows.
func BenchmarkRelayBufAcquire(b *testing.B) {
	b.Run("pooled", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			bp := relayBufPool.Get().(*[]byte)
			buf := *bp
			buf[0] = byte(i) // touch it so nothing is optimised away
			relayBufPool.Put(bp)
		}
	})
	b.Run("make-per-conn", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := make([]byte, relayBuf) // the old behaviour: a fresh buffer every connection
			buf[0] = byte(i)
			_ = buf
		}
	})
}
