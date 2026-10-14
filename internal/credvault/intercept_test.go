package credvault

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// proxyTo dials the handler as a client would dial the egress filter, and issues the CONNECT.
func proxyTo(t *testing.T, h http.Handler, target string) (net.Conn, *bufio.Reader) {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)

	br := bufio.NewReader(conn)

	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("CONNECT %s got %d: %s", target, resp.StatusCode, body)
	}

	return conn, br
}

// The whole flow, in one test: an agent speaks TLS to what it believes is the destination, and the
// request that comes out the other side carries the real credential instead of the placeholder the
// agent was given.
//
// The sandbox's own client trusts this authority and nothing else - it is the only certificate it
// is given - so a leaf it accepts is a leaf this vault's authority signed.
func TestATerminatedTunnelCarriesTheRealCredentialOut(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	// Stands in for the filter's relay. It is the point the request has reached the far side of
	// the termination, which is where the credential has to be real.
	got := make(chan *http.Request, 1)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// What the filter does here for real: the policy check, then the swap, then the relay.
		if err := v.Apply(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		got <- r

		_, _ = io.WriteString(w, "upstream says hello")
	})

	conn, _ := proxyTo(t, Intercept(inner, v), "api.example.com:443")

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(v.CA().PEM()) {
		t.Fatal("the vault's authority PEM did not parse: a sandbox could never trust it")
	}

	client := tls.Client(conn, &tls.Config{ServerName: "api.example.com", RootCAs: pool})
	if err := client.Handshake(); err != nil {
		t.Fatalf("the sandbox's client would not complete the handshake: %v", err)
	}

	defer client.Close()

	// Origin form, exactly as a client that believes it is talking to the destination sends it,
	// carrying the fake token it was given.
	fmt.Fprint(client, "GET /v1/models HTTP/1.1\r\nHost: api.example.com\r\n"+
		"Authorization: Bearer sk-placeholder-not-real\r\nConnection: close\r\n\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("reading the response inside the tunnel: %v", err)
	}

	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream says hello" {
		t.Errorf("the response did not come back through the tunnel: %q", body)
	}

	out := <-got

	if h := out.Header.Get("Authorization"); h != "Bearer sk-live-secret" {
		t.Errorf("the request left the tunnel with %q, want the real credential: the swap did "+
			"not happen, and the agent's placeholder went to the host", h)
	}

	// The scheme and host are put back from the CONNECT target, because the relay reads the URL
	// and a decrypted request carries neither.
	if out.URL.Scheme != "https" || out.URL.Host != "api.example.com:443" {
		t.Errorf("the request handed on was %s://%s, want https://api.example.com:443",
			out.URL.Scheme, out.URL.Host)
	}

	if out.URL.Path != "/v1/models" {
		t.Errorf("path is %q, want /v1/models", out.URL.Path)
	}
}

// A destination no binding covers keeps the tunnel it has always had. Terminating every CONNECT
// would make the filter a man in the middle of traffic it has no business reading, and the
// authority's own name constraints would refuse to certify it anyway.
func TestAnUnboundDestinationIsNotTerminated(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	reached := make(chan string, 1)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.Method + " " + r.Host

		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(Intercept(inner, v))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()

	fmt.Fprint(conn, "CONNECT unbound.example.com:443 HTTP/1.1\r\nHost: unbound.example.com:443\r\n\r\n")

	if got := <-reached; got != "CONNECT unbound.example.com:443" {
		t.Errorf("the inner handler saw %q: an unbound CONNECT must be handed on whole, so the "+
			"filter can splice it as it always has", got)
	}
}

// A plain request is not a tunnel and is never terminated: it goes straight to the filter, whose
// own relay path adds the credentials.
func TestAPlainRequestGoesStraightToTheFilter(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	reached := make(chan string, 1)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.Method

		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(Intercept(inner, v))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if got := <-reached; got != http.MethodGet {
		t.Errorf("the inner handler saw %q, want GET", got)
	}
}

// A nil vault is every sandbox without a credential vault, which is almost all of them. The
// handler must then be exactly the filter, so shipping this cannot change their data path.
func TestANilVaultIsTheFilterUntouched(t *testing.T) {
	reached := make(chan string, 1)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.Method + " " + r.Host

		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(Intercept(inner, nil))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()

	fmt.Fprint(conn, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n")

	if got := <-reached; got != "CONNECT api.example.com:443" {
		t.Errorf("with no vault the handler saw %q: it must hand every request to the filter", got)
	}
}
