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
)

// connectStatus sends one CONNECT through proxy and returns the status and body of the answer.
func connectStatus(t *testing.T, proxy, target string) (int, string) {
	t.Helper()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	// A 200 is an open tunnel whose body never ends; only a refusal has a body to read.
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, ""
	}

	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	return resp.StatusCode, string(body)
}

// A default-allow filter carried a CONNECT to any port: "CONNECT 1.1.1.1:53" answered 200 and
// spliced raw TCP, although SPEC.md promises HTTP and HTTPS only. The refusal must happen before
// any socket is opened, and must say which port and how to allow it.
func TestDefaultAllowRefusesPortsOtherThanHTTPAndHTTPS(t *testing.T) {
	dialled := false

	f := NewPolicy(Policy{DefaultAction: ActionAllow, Egress: []Rule{}})
	f.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		dialled = true
		return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
	}

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	for _, target := range []string{"1.1.1.1:53", "example.com:22", "example.com:8443", "[2606:4700::1111]:25"} {
		code, body := connectStatus(t, proxy.URL, target)
		if code != http.StatusForbidden {
			t.Errorf("CONNECT %s = %d, want 403", target, code)
		}

		_, port, _ := net.SplitHostPort(target)
		if !strings.Contains(body, "port "+port) || !strings.Contains(body, "egress_allow") {
			t.Errorf("CONNECT %s refusal %q does not name the port and how to allow it", target, body)
		}
	}

	if dialled {
		t.Error("a refused port was resolved: the refusal must come before any lookup or dial")
	}
}

// Plain HTTP to a port other than 80 is the same raw-port door in a different request shape.
func TestPlainHTTPToAnotherPortIsRefused(t *testing.T) {
	f := NewPolicy(Policy{DefaultAction: ActionAllow, Egress: []Rule{}})
	f.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return nil, fmt.Errorf("must not resolve")
	}

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	client := &http.Client{Transport: &http.Transport{Proxy: fixedProxy(t, proxy.URL)}}

	resp, err := client.Get("http://example.com:6379/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("plain HTTP to port 6379 = %d, want 403", resp.StatusCode)
	}
}

// An egress_allow entry written as host:port used to be accepted and its port silently dropped.
// It now permits exactly that port for that host (and its subdomains, as the entry always
// matched), on top of 80 and 443 - and no other port, and no other host on that port.
func TestAllowEntryWithAPortPermitsExactlyThatPort(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()

	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}

			_ = c.Close()
		}
	}()

	_, port, _ := net.SplitHostPort(echo.Addr().String())

	f := New([]string{"git.example:" + port, "127.0.0.1:" + port, "other.example"})
	f.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}

	// The allowed name resolves to loopback here, which the policy would refuse on its own; the
	// explicit IP entry opens it, so what is under test is only the port.
	proxy := httptest.NewServer(f)
	defer proxy.Close()

	for target, want := range map[string]int{
		"git.example:" + port:     http.StatusOK,        // the entry's host and port
		"ssh.git.example:" + port: http.StatusOK,        // a subdomain, as the entry matches
		"127.0.0.1:" + port:       http.StatusOK,        // an IP entry with a port
		"git.example:23":          http.StatusForbidden, // same host, another port
		"other.example:" + port:   http.StatusForbidden, // another allowed host, not granted the port
		"evilgit.example:" + port: http.StatusForbidden, // a suffix, not a subdomain
	} {
		if code, body := connectStatus(t, proxy.URL, target); code != want {
			t.Errorf("CONNECT %s = %d (%s), want %d", target, code, strings.TrimSpace(body), want)
		}
	}
}
