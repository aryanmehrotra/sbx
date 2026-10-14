package egress

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// claimsEverything is a credential source that says every destination is bound and records what
// the data path asked it.
type claimsEverything struct {
	asked   int
	applied int

	// refuse is what Apply returns, for the case where a credential cannot be added.
	refuse error
}

func (c *claimsEverything) Bound(string, string) bool {
	c.asked++

	return true
}

func (c *claimsEverything) Apply(r *http.Request) error {
	c.applied++

	if c.refuse != nil {
		return c.refuse
	}

	r.Header.Set("Authorization", "Bearer the-real-one")

	return nil
}

// A nil Credentials field is the container filter and every sandbox without a vault: the splice
// and the relay are what they always were. A source that claims every destination does not change
// where the sandbox may reach either - an allowed CONNECT still tunnels, a denied one is still 403
// and still never dialled, and a permitted plain request still relays its body back.
//
// What it does change is what a permitted request carries, which the tests below this one cover.
// The filter itself never terminates a tunnel: that is credvault.Intercept's job, in the package
// that holds the key, because this one is compiled into a container on the sandbox's own bridge.
func TestACredentialSourceDoesNotChangeWhereASandboxMayReach(t *testing.T) {
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

			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream says hello")
	}))
	defer plain.Close()

	host, port, _ := net.SplitHostPort(echo.Addr().String())
	plainHost, plainPort, _ := net.SplitHostPort(strings.TrimPrefix(plain.URL, "http://"))

	source := &claimsEverything{}

	for _, c := range []struct {
		name string
		with CredentialSource
	}{
		{"nil, as every filter is today", nil},
		{"a source that claims every destination", source},
	} {
		f := New([]string{host + ":" + port, plainHost + ":" + plainPort})
		f.Credentials = c.with

		proxy := httptest.NewServer(f)

		// An allowed CONNECT still splices.
		conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}

		fmt.Fprintf(conn, "CONNECT %s:%s HTTP/1.1\r\nHost: %s:%s\r\n\r\n", host, port, host, port)

		br := bufio.NewReader(conn)

		resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
		if err != nil {
			t.Fatal(err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: an allowed CONNECT got %d, want 200", c.name, resp.StatusCode)
		}

		fmt.Fprint(conn, "ping")

		got := make([]byte, 4)
		if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
			t.Fatalf("%s: the tunnel did not carry bytes (%q, %v): the splice is not what it was",
				c.name, got, err)
		}

		_ = conn.Close()

		// A denied CONNECT is still 403 and still never dialled.
		denied, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}

		fmt.Fprint(denied, "CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n")

		dresp, err := http.ReadResponse(bufio.NewReader(denied), &http.Request{Method: "CONNECT"})
		if err != nil {
			t.Fatal(err)
		}

		if dresp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: a denied CONNECT got %d, want 403", c.name, dresp.StatusCode)
		}

		_ = denied.Close()

		// And plain HTTP still relays, unread and unchanged.
		tr := &http.Transport{Proxy: fixedProxy(t, proxy.URL)}

		req, _ := http.NewRequest(http.MethodGet, plain.URL, nil)

		relayed, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}

		body, _ := io.ReadAll(relayed.Body)
		relayed.Body.Close()

		if string(body) != "upstream says hello" {
			t.Fatalf("%s: forward did not relay the body: %q", c.name, body)
		}

		tr.CloseIdleConnections()
		proxy.Close()
	}

	// The relay asks, because that is where a plain request gets its credentials. The tunnel does
	// not: the filter splices it, and only Intercept reads inside one.
	if source.asked == 0 {
		t.Error("the relay never asked the credential source whether the destination was bound")
	}
}

// The relay adds the credentials, after the policy check and before the wire. This is the plain
// HTTP half of the swap; the tunnel half is credvault.Intercept.
func TestTheRelayAddsCredentialsToAPermittedRequest(t *testing.T) {
	var carried string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		carried = r.Header.Get("Authorization")

		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	host, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))

	f := New([]string{host + ":" + port})
	source := &claimsEverything{}
	f.Credentials = source

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	tr := &http.Transport{Proxy: fixedProxy(t, proxy.URL)}
	defer tr.CloseIdleConnections()

	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Authorization", "Bearer sk-placeholder-not-real")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if source.applied != 1 {
		t.Fatalf("Apply was called %d times, want once", source.applied)
	}

	if carried != "Bearer the-real-one" {
		t.Errorf("upstream received %q, want the real credential: the agent's placeholder "+
			"reached the host", carried)
	}
}

// A credential that cannot be added means the request is not sent. Relaying it without the
// credential reaches the real host as the agent's own unauthenticated call, which fails there in a
// way that reads as the agent's bug rather than the filter's.
func TestTheRelayRefusesARequestWhoseCredentialsCouldNotBeAdded(t *testing.T) {
	var reached int

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++

		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	host, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))

	f := New([]string{host + ":" + port})
	f.Credentials = &claimsEverything{refuse: errNoCredential}

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	tr := &http.Transport{Proxy: fixedProxy(t, proxy.URL)}
	defer tr.CloseIdleConnections()

	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("a request whose credentials could not be added got %d, want 502",
			resp.StatusCode)
	}

	if reached != 0 {
		t.Error("the request was relayed anyway, without the credential it was supposed to carry")
	}
}

// errNoCredential stands in for whatever the vault could not do.
var errNoCredential = errors.New("no such credential")

// The container filter is built from a fixed list of this package's files, so a change here reaches
// a program running on a sandbox's own bridge. The seam must not take anything with it: no new
// import, nothing from internal/credvault, and the same list of files as before.
func TestTheSeamDoesNotReachIntoTheContainerFilter(t *testing.T) {
	files, err := BuildContext("golang:1.26-alpine", "alpine:3.20")
	if err != nil {
		t.Fatal(err)
	}

	// The list the Dockerfile copies, exactly: Dockerfile, go.mod and the five Go files.
	var names []string
	for name := range files {
		names = append(names, name)
	}

	want := map[string]bool{"Dockerfile": true, "go.mod": true, "main.go": true,
		"filter.go": true, "policy.go": true, "control.go": true, "doors.go": true}

	if len(names) != len(want) {
		t.Fatalf("the build context has %d files (%v), want the %d it has always had: a file "+
			"added to this package is compiled into the container filter", len(names), names, len(want))
	}

	for _, name := range names {
		if !want[name] {
			t.Errorf("the build context now carries %s", name)
		}
	}

	// The seam is in there, because filter.go is copied verbatim - and it brings nothing with it.
	if !strings.Contains(files["filter.go"], "CredentialSource") {
		t.Fatal("filter.go in the build context has no CredentialSource: the copy is not verbatim")
	}

	// The comments may name the vault - that is where the custody argument is written down - but
	// nothing may import it: the container filter has no module path to resolve it from, and the
	// key must never be in that process.
	for name, body := range files {
		if strings.Contains(body, "sbx/internal/credvault") {
			t.Errorf("%s imports credvault: the vault must not be compiled into the container "+
				"filter, because that process should never hold the key", name)
		}
	}

	// The interface uses only what filter.go already imports, so the generated program's imports
	// are the same ones. crypto/tls here would mean a TLS stack inside the container filter.
	for _, unwanted := range []string{"crypto/tls", "crypto/x509", "encoding/pem"} {
		if strings.Contains(files["filter.go"], `"`+unwanted+`"`) {
			t.Errorf("filter.go now imports %s: the seam was supposed to keep the crypto out of "+
				"this package", unwanted)
		}
	}
}
