package wsclient_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/wsclient"
	"github.com/aryanmehrotra/sbx/internal/wsclient/wstest"
)

// serve starts a test server whose handler gets the upgraded connection, and returns its ws URL.
// Server-side failures go to the errs channel rather than t, because the handler goroutine can
// outlive the test body.
func serve(t *testing.T, h func(c *wstest.Conn, r *http.Request)) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := wstest.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()

		h(c, r)
	}))
	t.Cleanup(srv.Close)

	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/channels?x=1"
}

func dial(t *testing.T, u string, opt wsclient.Options) *wsclient.Conn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := wsclient.Dial(ctx, u, opt)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// A client that fails to refuse something tends to block reading instead; the deadline turns
	// that into a failure with a message rather than a suite timeout.
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))

	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestEchoEveryLengthForm(t *testing.T) {
	// 125/126 and 65535/65536 are the boundaries of the three length encodings, in both
	// directions: the client writes each size and reads the server's echo back.
	sizes := []int{0, 1, 125, 126, 127, 65535, 65536, 300000}

	masks := make(chan bool, len(sizes))

	u := serve(t, func(c *wstest.Conn, _ *http.Request) {
		for {
			f, err := c.ReadFrame()
			if err != nil || f.Op == wstest.OpClose {
				return
			}

			masks <- f.Masked
			_ = c.WriteFrame(true, f.Op, f.Payload)
		}
	})

	c := dial(t, u, wsclient.Options{})

	for _, n := range sizes {
		msg := bytes.Repeat([]byte("a"), n)
		if err := c.WriteText(msg); err != nil {
			t.Fatalf("write %d: %v", n, err)
		}

		typ, got, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read %d: %v", n, err)
		}

		if typ != wsclient.TextMessage || !bytes.Equal(got, msg) {
			t.Fatalf("size %d: got type %d len %d", n, typ, len(got))
		}

		if !<-masks {
			t.Fatalf("size %d: client frame was not masked", n)
		}
	}
}

func TestUpgradeCarriesHeadersAndPath(t *testing.T) {
	got := make(chan *http.Request, 1)

	u := serve(t, func(_ *wstest.Conn, r *http.Request) { got <- r })

	dial(t, u, wsclient.Options{Header: http.Header{
		"Authorization": {"token abc"},
		// A caller-supplied handshake header must not replace ours.
		"Sec-WebSocket-Version": {"8"},
	}})

	r := <-got
	if r.Header.Get("Authorization") != "token abc" {
		t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
	}

	if r.URL.Path != "/channels" || r.URL.RawQuery != "x=1" {
		t.Fatalf("request URI = %s", r.URL.RequestURI())
	}

	if v := r.Header.Values("Sec-WebSocket-Version"); len(v) != 1 || v[0] != "13" {
		t.Fatalf("Sec-WebSocket-Version = %v", v)
	}
}

func TestFragmentedMessageWithPingBetweenFragments(t *testing.T) {
	pong := make(chan []byte, 1)

	u := serve(t, func(c *wstest.Conn, _ *http.Request) {
		_ = c.WriteFrame(false, wstest.OpText, []byte("hel"))
		_ = c.WriteFrame(true, wstest.OpPing, []byte("are you there"))
		_ = c.WriteFrame(false, wstest.OpContinuation, []byte("lo "))
		_ = c.WriteFrame(true, wstest.OpContinuation, []byte("world"))

		f, err := c.ReadFrame()
		if err == nil && f.Op == wstest.OpPong {
			pong <- f.Payload
		}

		_, _ = c.ReadFrame()
	})

	c := dial(t, u, wsclient.Options{})

	_, got, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "hello world" {
		t.Fatalf("reassembled %q", got)
	}

	select {
	case p := <-pong:
		if string(p) != "are you there" {
			t.Fatalf("pong payload %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no pong for a ping sent between fragments")
	}
}

func TestServerCloseIsReportedAndEchoed(t *testing.T) {
	echoed := make(chan wstest.Frame, 1)

	u := serve(t, func(c *wstest.Conn, _ *http.Request) {
		_ = c.CloseWith(4001, "kernel gone")

		f, err := c.ReadFrame()
		if err == nil {
			echoed <- f
		}
	})

	c := dial(t, u, wsclient.Options{})

	_, _, err := c.ReadMessage()

	var ce *wsclient.CloseError
	if !errors.As(err, &ce) || ce.Code != 4001 || ce.Text != "kernel gone" {
		t.Fatalf("err = %v", err)
	}

	f := <-echoed
	if f.Op != wstest.OpClose || binary.BigEndian.Uint16(f.Payload) != 4001 {
		t.Fatalf("echo = %+v", f)
	}

	if err := c.WriteText([]byte("late")); !errors.Is(err, wsclient.ErrClosed) {
		t.Fatalf("write after close = %v, want ErrClosed", err)
	}
}

// Each case sends one malformed sequence; the client must refuse it and tell the server why
// with a close frame carrying the right status.
func TestProtocolViolations(t *testing.T) {
	cases := []struct {
		name     string
		send     func(c *wstest.Conn)
		max      int64
		wantCode uint16
		wantErr  string
	}{
		{"masked server frame", func(c *wstest.Conn) { _ = c.WriteMaskedFrame(wstest.OpText, []byte("x")) }, 0, 1002, "masked"},
		{"reserved bit", func(c *wstest.Conn) { _ = c.WriteFrameRSV(wstest.OpText, []byte("x"), 0x4) }, 0, 1002, "reserved"},
		{"fragmented ping", func(c *wstest.Conn) { _ = c.WriteFrame(false, wstest.OpPing, nil) }, 0, 1002, "control frame"},
		{"oversized ping", func(c *wstest.Conn) { _ = c.WriteFrame(true, wstest.OpPing, make([]byte, 126)) }, 0, 1002, "control frame"},
		{"orphan continuation", func(c *wstest.Conn) { _ = c.WriteFrame(true, wstest.OpContinuation, []byte("x")) }, 0, 1002, "continuation"},
		{"data inside fragment", func(c *wstest.Conn) {
			_ = c.WriteFrame(false, wstest.OpText, []byte("a"))
			_ = c.WriteFrame(true, wstest.OpText, []byte("b"))
		}, 0, 1002, "inside a fragmented"},
		{"unknown opcode", func(c *wstest.Conn) { _ = c.WriteFrame(true, 0x3, nil) }, 0, 1002, "unknown opcode"},
		{"invalid utf8", func(c *wstest.Conn) { _ = c.WriteText([]byte{0xff, 0xfe}) }, 0, 1007, "UTF-8"},
		{"declared length over limit", func(c *wstest.Conn) {
			// Header claims 2000 bytes; only the header is sent, so a client that allocates
			// before checking would block reading rather than fail.
			_, _ = c.Write([]byte{0x81, 126, 0x07, 0xd0})
		}, 1024, 1009, "limit"},
		{"fragments over limit", func(c *wstest.Conn) {
			_ = c.WriteFrame(false, wstest.OpBinary, make([]byte, 600))
			_ = c.WriteFrame(true, wstest.OpContinuation, make([]byte, 600))
		}, 1024, 1009, "limit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			closeCode := make(chan uint16, 1)

			u := serve(t, func(c *wstest.Conn, _ *http.Request) {
				tc.send(c)

				for {
					f, err := c.ReadFrame()
					if err != nil {
						closeCode <- 0

						return
					}

					if f.Op == wstest.OpClose && len(f.Payload) >= 2 {
						closeCode <- binary.BigEndian.Uint16(f.Payload)

						return
					}
				}
			})

			c := dial(t, u, wsclient.Options{MaxMessage: tc.max})

			_, _, err := c.ReadMessage()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}

			if got := <-closeCode; got != tc.wantCode {
				t.Fatalf("close code = %d, want %d", got, tc.wantCode)
			}
		})
	}
}

func TestRefusedUpgrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid token", http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := wsclient.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), wsclient.Options{})

	var he *wsclient.HandshakeError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}

	if !strings.Contains(err.Error(), "token") || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("message does not say what to check: %v", err)
	}
}

func TestWrongAcceptIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, rw, _ := hj.Hijack()
		defer conn.Close()

		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n" +
			"Connection: Upgrade\r\nSec-WebSocket-Accept: bm90IHRoZSByaWdodCBoYXNo\r\n\r\n")
		_ = rw.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	_, err := wsclient.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), wsclient.Options{})
	if err == nil || !strings.Contains(err.Error(), "Accept") {
		t.Fatalf("err = %v", err)
	}
}

func TestDialHonoursContextWhenServerNeverAnswers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and say nothing: the handshake read would block forever without a bound.
			defer c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()

	_, err = wsclient.Dial(ctx, "ws://"+ln.Addr().String()+"/", wsclient.Options{})
	if err == nil {
		t.Fatal("dial to a silent server succeeded")
	}

	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("dial took %s after a 200ms deadline", el)
	}

	// And a cancel with no deadline at all.
	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel2)

	if _, err := wsclient.Dial(ctx2, "ws://"+ln.Addr().String()+"/", wsclient.Options{}); err == nil {
		t.Fatal("dial survived a cancel")
	}
}

func TestDroppedConnectionIsUnexpectedEOF(t *testing.T) {
	u := serve(t, func(c *wstest.Conn, _ *http.Request) {
		_ = c.WriteFrame(false, wstest.OpText, []byte("half"))
		// return: the deferred Close drops TCP mid-message with no close frame.
	})

	c := dial(t, u, wsclient.Options{})

	_, _, err := c.ReadMessage()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want ErrUnexpectedEOF", err)
	}
}

func TestClientCloseSendsNormalClosure(t *testing.T) {
	got := make(chan wstest.Frame, 1)

	u := serve(t, func(c *wstest.Conn, _ *http.Request) {
		f, err := c.ReadFrame()
		if err == nil {
			got <- f
		}
	})

	c := dial(t, u, wsclient.Options{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	f := <-got
	if f.Op != wstest.OpClose || binary.BigEndian.Uint16(f.Payload) != wsclient.CloseNormal {
		t.Fatalf("frame = %+v", f)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	const writers, each = 8, 50

	received := make(chan string, writers*each)

	u := serve(t, func(c *wstest.Conn, _ *http.Request) {
		for {
			_, p, err := c.ReadMessage()
			if err != nil {
				close(received)

				return
			}

			received <- string(p)
		}
	})

	c := dial(t, u, wsclient.Options{})

	var wg sync.WaitGroup

	for w := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range each {
				// Large enough to span several TCP writes if two headers ever interleaved.
				msg := fmt.Sprintf("%d-%d-%s", w, i, strings.Repeat("z", 3000))
				if err := c.WriteText([]byte(msg)); err != nil {
					t.Errorf("write: %v", err)

					return
				}

				if i%10 == 0 {
					_ = c.Ping([]byte("k"))
				}
			}
		}()
	}

	wg.Wait()
	_ = c.Close()

	n := 0

	for m := range received {
		if !strings.HasSuffix(m, strings.Repeat("z", 3000)) {
			t.Fatalf("corrupted message prefix %q", m[:min(len(m), 20)])
		}
		n++
	}

	if n != writers*each {
		t.Fatalf("received %d messages, want %d", n, writers*each)
	}
}

func TestSecureDial(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := wstest.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()

		_, p, err := c.ReadMessage()
		if err == nil {
			_ = c.WriteText(p)
		}
	}))
	defer srv.Close()

	tlsCfg := srv.Client().Transport.(*http.Transport).TLSClientConfig

	c := dial(t, "wss"+strings.TrimPrefix(srv.URL, "https"), wsclient.Options{TLSConfig: tlsCfg})
	if err := c.WriteText([]byte("over tls")); err != nil {
		t.Fatal(err)
	}

	_, p, err := c.ReadMessage()
	if err != nil || string(p) != "over tls" {
		t.Fatalf("got %q, %v", p, err)
	}
}

func TestBadScheme(t *testing.T) {
	_, err := wsclient.Dial(context.Background(), "http://localhost/", wsclient.Options{})
	if err == nil || !strings.Contains(err.Error(), "ws or wss") {
		t.Fatalf("err = %v", err)
	}
}
