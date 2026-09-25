package wsserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/wsclient"
)

// echo upgrades and echoes every message with its type, closing with 4001 on the text "bye".
func echoServer(t *testing.T, opt Options) *httptest.Server {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, opt)
		if err != nil {
			return
		}
		defer c.Close()

		for {
			typ, p, err := c.ReadMessage()
			if err != nil {
				return
			}

			if typ == TextMessage && string(p) == "bye" {
				_ = c.CloseWith(4001, "TAKEN_OVER")
				return
			}

			if err := c.WriteMessage(typ, p); err != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.Close)

	return ts
}

func dial(t *testing.T, ts *httptest.Server) *wsclient.Conn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := wsclient.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), wsclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestEchoKeepsMessageTypes(t *testing.T) {
	c := dial(t, echoServer(t, Options{}))

	big := strings.Repeat("x", 70000) // exercises the 8-byte length form

	for _, m := range []struct {
		typ wsclient.MessageType
		p   string
	}{
		{wsclient.TextMessage, `{"type":"ping"}`},
		{wsclient.BinaryMessage, "\x00ls\n"},
		{wsclient.BinaryMessage, strings.Repeat("y", 300)},
		{wsclient.TextMessage, big},
	} {
		if err := c.WriteMessage(m.typ, []byte(m.p)); err != nil {
			t.Fatal(err)
		}

		typ, p, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}

		if typ != m.typ || string(p) != m.p {
			t.Fatalf("echo: got type %d len %d, want type %d len %d", typ, len(p), m.typ, len(m.p))
		}
	}

	// A ping from the client is answered inside ReadMessage and does not surface as data.
	if err := c.Ping(nil); err != nil {
		t.Fatal(err)
	}

	if err := c.WriteText([]byte("after-ping")); err != nil {
		t.Fatal(err)
	}

	if _, p, err := c.ReadMessage(); err != nil || string(p) != "after-ping" {
		t.Fatalf("after ping: %q %v", p, err)
	}
}

func TestCloseCodeReachesTheClient(t *testing.T) {
	c := dial(t, echoServer(t, Options{}))

	if err := c.WriteText([]byte("bye")); err != nil {
		t.Fatal(err)
	}

	_, _, err := c.ReadMessage()

	var ce *wsclient.CloseError
	if !errors.As(err, &ce) || ce.Code != 4001 || ce.Text != "TAKEN_OVER" {
		t.Fatalf("got %v, want close 4001 TAKEN_OVER", err)
	}
}

func TestNotAnUpgradeIs400(t *testing.T) {
	ts := echoServer(t, Options{})

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

// rawHandshake opens a TCP connection and completes the handshake by hand, so a test can send
// frames a well-behaved client never would.
func rawHandshake(t *testing.T, ts *httptest.Server) (net.Conn, *bufio.Reader) {
	t.Helper()

	nc, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	fmt.Fprintf(nc, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")

	br := bufio.NewReader(nc)

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status %d", resp.StatusCode)
	}

	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept %q is not the RFC 6455 example value", got)
	}

	return nc, br
}

// readCloseCode reads one server frame and returns its close code, failing on anything else.
func readCloseCode(t *testing.T, nc net.Conn, br *bufio.Reader) int {
	t.Helper()

	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))

	var head [2]byte
	if _, err := br.Read(head[:1]); err != nil {
		t.Fatal(err)
	}

	if _, err := br.Read(head[1:]); err != nil {
		t.Fatal(err)
	}

	if head[0]&0x0f != opClose {
		t.Fatalf("server sent opcode %#x, want close", head[0]&0x0f)
	}

	p := make([]byte, head[1]&0x7f)
	if _, err := br.Read(p); err != nil || len(p) < 2 {
		t.Fatalf("close payload %v %v", p, err)
	}

	return int(p[0])<<8 | int(p[1])
}

func TestUnmaskedClientFrameFailsTheConnection(t *testing.T) {
	nc, br := rawHandshake(t, echoServer(t, Options{}))

	_, _ = nc.Write([]byte{0x81, 0x02, 'h', 'i'}) // text "hi", mask bit clear

	if code := readCloseCode(t, nc, br); code != CloseProtocolError {
		t.Fatalf("close code %d, want %d", code, CloseProtocolError)
	}
}

func TestDeclaredLengthOverTheCapIsRefusedBeforeReading(t *testing.T) {
	nc, br := rawHandshake(t, echoServer(t, Options{MaxMessage: 1024}))

	// Declares 1 GiB and sends none of it: the server must refuse on the header alone.
	_, _ = nc.Write([]byte{0x82, 0xff, 0, 0, 0, 0, 0x40, 0, 0, 0, 1, 2, 3, 4})

	if code := readCloseCode(t, nc, br); code != CloseTooBig {
		t.Fatalf("close code %d, want %d", code, CloseTooBig)
	}
}
