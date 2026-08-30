package egress

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEgressPermits(t *testing.T) {
	f := New([]string{"openai.com", "PyPI.org:443", "10.0.0.5", " "})

	cases := []struct {
		host string
		want bool
	}{
		{"openai.com", true},         // the host itself
		{"api.openai.com", true},     // a subdomain
		{"api.openai.com:443", true}, // host:port
		{"API.OpenAI.com", true},     // case-insensitive
		{"pypi.org", true},           // the port on the allow entry is ignored
		{"files.pythonhosted.org", false},
		{"notopenai.com", false},       // not a subdomain, a different host that ends the same
		{"openai.com.evil.com", false}, // the classic suffix-attack - must NOT match
		{"evil.com", false},
		{"10.0.0.5", true},
		{"10.0.0.6", false},
	}

	for _, c := range cases {
		if got := f.Permits(c.host); got != c.want {
			t.Errorf("Permits(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// A denied CONNECT must get 403 and NO socket to the target - the proxy refuses before dialling.
func TestEgressDeniedConnectIsRefused(t *testing.T) {
	proxy := httptest.NewServer(New([]string{"openai.com"}))
	defer proxy.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprint(conn, "CONNECT evil.com:443 HTTP/1.1\r\nHost: evil.com:443\r\n\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied CONNECT got %d, want 403", resp.StatusCode)
	}
}

// An allowed CONNECT must tunnel end to end. The allowed "host" is a local echo listener, so the
// test proves the proxy actually splices bytes to a permitted destination.
func TestEgressAllowedConnectTunnels(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()

	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c) // echo
	}()

	proxy := httptest.NewServer(New([]string{"127.0.0.1"}))
	defer proxy.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo.Addr(), echo.Addr())

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed CONNECT got %d, want 200", resp.StatusCode)
	}

	// The tunnel is open; the echo server returns whatever we send.
	fmt.Fprint(conn, "ping")

	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("reading through the tunnel: %v", err)
	}

	if string(buf) != "ping" {
		t.Errorf("tunnel echoed %q, want ping", buf)
	}
}

func TestEgressDeniedPlainHTTPIsRefused(t *testing.T) {
	proxy := httptest.NewServer(New([]string{"openai.com"}))
	defer proxy.Close()

	req, _ := http.NewRequest("GET", "http://evil.com/", nil)
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_ = req.Write(conn)

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied plain HTTP got %d, want 403", resp.StatusCode)
	}
}
