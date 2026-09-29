package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// slowLoopback resolves every name to 127.0.0.1 after a delay, and gives up early if its context
// ends first - as the system resolver does. The delay is what opens the window: net/http cancels a
// request's context the moment its background read sees the client's FIN, which is long before a
// real lookup comes back.
func slowLoopback(ctx context.Context, _ string) ([]netip.Addr, error) {
	select {
	case <-time.After(200 * time.Millisecond):
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// halfClosed dials the proxy, writes raw, and shuts its write side, as busybox wget does after
// sending a request. The read side stays open for the answer.
func halfClosed(t *testing.T, proxyURL, raw string) *bufio.Reader {
	t.Helper()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}

	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	return bufio.NewReader(conn)
}

// A client that half-closes after its request is finished sending, not gone. busybox wget - the
// wget in every alpine image - sends "GET https://host/" to its proxy and then shuts its write
// side; the filter refused every such request with 502 "lookup ...: operation was canceled",
// because it resolved on the request's context and net/http had already cancelled it.
func TestEgressPlainHTTPSurvivesClientHalfClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "reached")
	}))
	defer upstream.Close()

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))

	f := New([]string{"upstream.test:" + port, "127.0.0.1"})
	f.Resolve = slowLoopback

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	target := fmt.Sprintf("http://upstream.test:%s/", port)
	br := halfClosed(t, proxy.URL, "GET "+target+" HTTP/1.1\r\nHost: upstream.test:"+port+"\r\nConnection: close\r\n\r\n")

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(body) != "reached" {
		t.Fatalf("half-closed client got %d %q, want 200 \"reached\"", resp.StatusCode, body)
	}
}

// The same for CONNECT: a tunnel is admitted and dialled before the client has anything more to
// say, so its FIN in that window is not a reason to refuse.
func TestEgressConnectSurvivesClientHalfClose(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()

	go func() {
		c, err := echo.Accept()
		if err == nil {
			_ = c.Close()
		}
	}()

	_, port, _ := net.SplitHostPort(echo.Addr().String())

	f := New([]string{"upstream.test:" + port, "127.0.0.1"})
	f.Resolve = slowLoopback

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	target := "upstream.test:" + port
	br := halfClosed(t, proxy.URL, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")

	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("half-closed CONNECT got %d %q, want 200", resp.StatusCode, body)
	}
}
