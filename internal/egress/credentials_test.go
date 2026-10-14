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

// claimsEverything is a credential source that says every destination is bound. It exists to prove
// that the data path does not ask: the filter must behave identically with it and without it,
// because reading inside a tunnel is the next piece of work and not this one.
type claimsEverything struct{ asked int }

func (c *claimsEverything) Bound(string, string) bool {
	c.asked++

	return true
}

// A nil Credentials field is today's filter, byte for byte on the path: the splice and the relay
// are unchanged, and nothing in tunnel or forward consults the field. A source that claims every
// destination changes nothing either, and is never even asked.
//
// This is what makes the field safe to ship before the behaviour behind it: the container filter
// compiles the same sources with a nil field, and a filter with a vault still carries exactly what
// its policy permits and refuses the rest.
func TestACredentialSourceDoesNotChangeTheDataPath(t *testing.T) {
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

	if source.asked != 0 {
		t.Fatalf("the data path asked the credential source %d times: swapping a tunnel for a "+
			"read is the next piece of work, and a field that is consulted now is behaviour "+
			"shipped without the tests for it", source.asked)
	}
}

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
