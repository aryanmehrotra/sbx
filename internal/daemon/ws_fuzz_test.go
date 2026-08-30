package daemon

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// The frame parser is the one part of sbx an unauthenticated stranger can feed bytes to at
// speed, so it is fuzzed rather than only exampled.
//
// `sbx serve --connect-addr` is the only surface reachable from off the machine, and readOne
// parses attacker-controlled lengths, masks, opcodes and reserved bits before anything has been
// relayed. The existing tests cover the cases somebody thought of - an unmasked client frame, a
// declared length of nine exabytes, a close mid-fragment - and this covers the ones nobody did.
//
// The contract is narrow and total: for ANY input, readFrame either returns a payload or an
// error. It must not panic, must not allocate on a declared length it has not been sent, and
// must not block forever on a truncated frame.
func FuzzWebSocketFrameParser(f *testing.F) {
	// A well-formed masked text frame, "hi".
	f.Add([]byte{0x81, 0x82, 0x01, 0x02, 0x03, 0x04, 'h' ^ 0x01, 'i' ^ 0x02})
	// A close frame.
	f.Add([]byte{0x88, 0x80, 0, 0, 0, 0})
	// A ping between fragments.
	f.Add([]byte{0x01, 0x80, 0, 0, 0, 0, 0x89, 0x80, 0, 0, 0, 0, 0x80, 0x80, 0, 0, 0, 0})
	// An unmasked client frame, which RFC 6455 requires a server to reject.
	f.Add([]byte{0x81, 0x02, 'h', 'i'})
	// A reserved bit set.
	f.Add([]byte{0xC1, 0x80, 0, 0, 0, 0})
	// A 64-bit length declaring far more than was sent.
	f.Add([]byte{0x81, 0xFF, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	// A 16-bit length form.
	f.Add([]byte{0x81, 0xFE, 0x00, 0x04, 0, 0, 0, 0, 'a', 'b', 'c', 'd'})
	// Empty, and a lone header byte.
	f.Add([]byte{})
	f.Add([]byte{0x81})

	f.Fuzz(func(t *testing.T, data []byte) {
		// A net.Pipe end that is already closed for writing gives readOne a real net.Conn to
		// fail against rather than an EOF-only reader, so the deadline and close paths are
		// exercised the way they are in production.
		client, server := net.Pipe()

		defer func() { _ = client.Close() }()
		defer func() { _ = server.Close() }()

		// Drained, because net.Pipe is unbuffered: readFrame answers a ping by writing a pong,
		// and with nobody reading the peer that write blocks forever. A real socket has a buffer.
		go func() {
			_, _ = io.Copy(io.Discard, client)
		}()

		c := &wsConn{conn: server, br: bufio.NewReader(bytes.NewReader(data))}
		c.lastPong = time.Now()

		// Bounded: a parser that blocks forever on a truncated frame is a denial of service,
		// and the fuzzer would otherwise report it as a timeout with no detail.
		done := make(chan struct{})

		go func() {
			defer close(done)

			// Whatever it returns is fine. Panicking, or allocating a declared nine exabytes,
			// is not - the first crashes the daemon, the second is the OOM this parser's
			// length checks exist to prevent.
			for range 4 {
				if _, err := c.readFrame(); err != nil {
					return
				}
			}
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("readFrame did not return within 5s on %d bytes - a truncated or hostile "+
				"frame must fail, not hang", len(data))
		}
	})
}

// parseFront reads a flag a deployment sets from an environment variable, so a typo in a
// platform's dashboard reaches it directly. It must reject rather than misparse: a port map
// with a hole in it silently fronts the wrong thing.
func FuzzParseFront(f *testing.F) {
	for _, s := range []string{
		"5432", "5432,6379", "db=5432,cache=6379", " db = 5432 ", "",
		"=", "db=", "=5432", "db=0", "db=65536", "db=-1", "db=99999999999999999999",
		"a=1,a=2", ",,,", "db=5432,", "db=5432,,cache=6379",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, spec string) {
		got, err := parseFront(spec)
		if err != nil {
			return // refusing is always a valid answer
		}

		// Whatever it accepted has to be a usable map: every port dialable, every name non-empty.
		for port, fr := range got {
			if port < 1 || port > 65535 {
				t.Errorf("parseFront(%q) accepted port %d, which cannot be dialled", spec, port)
			}

			if fr.name == "" {
				t.Errorf("parseFront(%q) accepted an unnamed front on port %d", spec, port)
			}

			if fr.port != port {
				t.Errorf("parseFront(%q) keyed port %d under %d", spec, fr.port, port)
			}
		}
	})
}
