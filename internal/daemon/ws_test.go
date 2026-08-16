package daemon

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The handshake vector from RFC 6455 section 1.3. If this is wrong nothing else matters,
// because no proxy will pass the connection through.
func TestHandshakeAcceptMatchesTheRFCVector(t *testing.T) {
	if got, want := wsAccept("dGhlIHNhbXBsZSBub25jZQ=="), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Errorf("wsAccept = %q, want the RFC's %q", got, want)
	}
}

// maskedFrame builds what a client would actually send.
func maskedFrame(opcode byte, payload []byte, final bool) []byte {
	head := []byte{opcode}
	if final {
		head[0] |= 0x80
	}

	mask := []byte{0xde, 0xad, 0xbe, 0xef}

	switch n := len(payload); {
	case n < 126:
		head = append(head, byte(n)|0x80)
	case n < 1<<16:
		head = append(head, 126|0x80, 0, 0)
		binary.BigEndian.PutUint16(head[2:], uint16(n))
	default:
		head = append(head, 127|0x80, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}

	head = append(head, mask...)

	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}

	return append(head, masked...)
}

// readerOn feeds bytes to a wsConn and keeps draining the other end, because answering a ping
// means writing a pong back down the same pipe - a helper that closes after writing turns a
// correct pong into a test failure.
func readerOn(b []byte) *wsConn {
	client, server := net.Pipe()

	go func() {
		_, _ = client.Write(b)

		// Drain whatever the server writes (pongs) until the test is done with it.
		buf := make([]byte, 1024)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()

	return &wsConn{conn: server, br: bufio.NewReader(server)}
}

// Every length form, because 126 and 127 switch the header's shape and an off-by-one there
// desynchronises the stream rather than failing cleanly.
func TestEveryLengthForm(t *testing.T) {
	for _, n := range []int{0, 5, 125, 126, 300, 1 << 16, (1 << 16) + 1} {
		payload := bytes.Repeat([]byte{'x'}, n)

		got, err := readerOn(maskedFrame(opBinary, payload, true)).readFrame()
		if err != nil {
			t.Fatalf("a %d byte frame failed: %v", n, err)
		}

		if len(got) != n {
			t.Errorf("a %d byte frame came back as %d bytes", n, len(got))
		}
	}
}

// A message split across frames, with a ping arriving between them - which RFC 6455 allows
// and a naive reassembler treats as data.
func TestFragmentsReassembleAndAPingBetweenThemDoesNotCorruptThem(t *testing.T) {
	var wire []byte
	wire = append(wire, maskedFrame(opBinary, []byte("hello "), false)...)
	wire = append(wire, maskedFrame(opPing, nil, true)...)
	wire = append(wire, maskedFrame(opContinuation, []byte("world"), true)...)

	got, err := readerOn(wire).readFrame()
	if err != nil {
		t.Fatalf("reassembly failed: %v", err)
	}

	if string(got) != "hello world" {
		t.Errorf("reassembled %q, want %q", got, "hello world")
	}
}

// The adversarial cases. This parser is reachable by anything holding the token.
func TestMalformedFramesAreRefused(t *testing.T) {
	unmasked := []byte{0x80 | opBinary, 0x03, 'a', 'b', 'c'} // masked bit clear

	huge := []byte{0x80 | opBinary, 127 | 0x80}
	huge = append(huge, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff) // ~9 exabytes
	huge = append(huge, 0xde, 0xad, 0xbe, 0xef)

	reserved := maskedFrame(opBinary, []byte("x"), true)
	reserved[0] |= 0x40 // RSV1 with no extension negotiated

	badop := maskedFrame(0x5, []byte("x"), true)

	for name, wire := range map[string][]byte{
		"unmasked client frame":  unmasked,
		"absurd declared length": huge,
		"reserved bit set":       reserved,
		"unknown opcode":         badop,
	} {
		if _, err := readerOn(wire).readFrame(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A close is an ordinary end, not an error to log.
func TestCloseEndsTheStream(t *testing.T) {
	if _, err := readerOn(maskedFrame(opClose, nil, true)).readFrame(); err != errClosed {
		t.Errorf("a close frame gave %v, want errClosed", err)
	}
}

// The ping exists to notice a connection that is dead without having said so. If nothing
// checks that the pong came back it notices nothing at all.
func TestKeepaliveGivesUpWhenThePongStops(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	c := &wsConn{conn: server, br: bufio.NewReader(server)}
	c.lastPong = time.Now().Add(-time.Hour) // the peer stopped answering long ago

	go func() { _, _ = client.Read(make([]byte, 64)) }()

	done := make(chan struct{})
	closed := make(chan struct{})

	go func() { c.keepalive(5*time.Millisecond, time.Second, done); close(closed) }()

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		close(done)
		t.Fatal("the keepalive never gave up on a peer that stopped answering")
	}
}

// Two goroutines write to one connection: the relay and the pinger. net.Conn.Write is not safe
// for concurrent use, and interleaved headers corrupt the stream in a way that reads as a bug
// somewhere else. Run under -race.
func TestConcurrentWritersDoNotInterleave(t *testing.T) {
	client, server := net.Pipe()

	c := &wsConn{conn: server, br: bufio.NewReader(server)}

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()

	done := make(chan struct{})

	for range 4 {
		go func() {
			for range 50 {
				_ = c.write(opBinary, bytes.Repeat([]byte{'a'}, 200))
			}
			done <- struct{}{}
		}()
	}

	go func() {
		for range 50 {
			_ = c.write(opPing, nil)
		}
		done <- struct{}{}
	}()

	for range 5 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a writer blocked")
		}
	}

	_ = client.Close()
	_ = server.Close()
}

func TestUpgradeRejectsWhatItCannotSpeak(t *testing.T) {
	for name, h := range map[string]map[string]string{
		"no upgrade header": {"Connection": "upgrade", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "x"},
		"wrong version":     {"Upgrade": "websocket", "Connection": "upgrade", "Sec-WebSocket-Version": "8", "Sec-WebSocket-Key": "x"},
		"no key":            {"Upgrade": "websocket", "Connection": "upgrade", "Sec-WebSocket-Version": "13"},
	} {
		r := newRequestWith(h)

		if _, err := wsUpgrade(nonHijacker{}, r); err == nil || strings.Contains(err.Error(), "hijack") {
			// reaching the hijack check means the header checks passed, which they must not
			t.Errorf("%s was accepted by the handshake checks", name)
		}
	}
}

// nonHijacker is an http.ResponseWriter that cannot be hijacked, so the handshake's header
// checks are reached without needing a real connection.
type nonHijacker struct{ h http.Header }

func (n nonHijacker) Header() http.Header {
	if n.h == nil {
		n.h = http.Header{}
	}

	return n.h
}
func (nonHijacker) Write([]byte) (int, error) { return 0, nil }
func (nonHijacker) WriteHeader(int)           {}

func newRequestWith(h map[string]string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/v1/connect", nil)
	for k, v := range h {
		r.Header.Set(k, v)
	}

	return r
}
