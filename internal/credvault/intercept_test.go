package credvault

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
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

// tlsThrough opens one tunnel and hands back the client's side of the TLS inside it, handshaken.
// cfg is the sandbox's own client configuration, so a test can send the SNI it likes - or none.
func tlsThrough(t *testing.T, h http.Handler, target string, cfg *tls.Config) *tls.Conn {
	t.Helper()

	conn, _ := proxyTo(t, h, target)

	client := tls.Client(conn, cfg)
	if err := client.Handshake(); err != nil {
		t.Fatalf("the sandbox's client would not complete the handshake: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return client
}

// h2Through is a client that reaches the handler the way an agent CLI does: CONNECT to the filter,
// TLS inside that, and HTTP/2 if the handshake agrees on it.
//
// ForceAttemptHTTP2 is not optional here. net/http disables HTTP/2 on any transport with a custom
// TLS dialer unless it is asked for explicitly, and a tunnel needs one - so without it this would
// test HTTP/1.1 twice and say h2 worked.
func h2Through(t *testing.T, h http.Handler, v *Vault, target string) *http.Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	host, _ := targetOf(target)
	cfg := trusting(t, v, host)
	cfg.NextProtos = []string{"h2", "http/1.1"}

	tr := &http.Transport{
		ForceAttemptHTTP2: true,
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
			if err != nil {
				return nil, err
			}

			fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)

			resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
			if err != nil {
				_ = conn.Close()

				return nil, err
			}

			if resp.StatusCode != http.StatusOK {
				_ = conn.Close()

				return nil, fmt.Errorf("CONNECT %s answered %d", target, resp.StatusCode)
			}

			inside := tls.Client(conn, cfg.Clone())
			if err := inside.HandshakeContext(ctx); err != nil {
				_ = conn.Close()

				return nil, err
			}

			return inside, nil
		},
	}

	// Registered after the server's, so it runs first: httptest's Close waits for the handler,
	// and the handler is inside serveTerminated until the client's end of the tunnel goes.
	t.Cleanup(tr.CloseIdleConnections)

	return &http.Client{Transport: tr}
}

// trusting is the sandbox's client configuration: this vault's authority and nothing else, which
// is the only certificate a sandbox is given.
func trusting(t *testing.T, v *Vault, serverName string) *tls.Config {
	t.Helper()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(v.CA().PEM()) {
		t.Fatal("the vault's authority PEM did not parse: a sandbox could never trust it")
	}

	return &tls.Config{ServerName: serverName, RootCAs: pool}
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

// The same flow over HTTP/2, which is how the clients this exists for actually talk.
//
// Claude Code and Codex stream over h2. A termination that only spoke HTTP/1.1 meant the credential
// swap had never been exercised by either of them: the handshake offered no h2, so the best case
// was a client quietly falling back and the worst was one that does not. The protocol is asserted
// on both sides - h2 into the tunnel, HTTP/1.1 out of it, because the relay writes HTTP/1.1
// upstream and a request struct that still said HTTP/2.0 would describe a wire nobody wrote.
func TestATerminatedTunnelCarriesTheRealCredentialOutOverHTTP2(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	got := make(chan *http.Request, 1)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := v.Apply(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		got <- r

		_, _ = io.WriteString(w, "upstream says hello")
	})

	client := h2Through(t, Intercept(inner, v), v, "api.example.com:443")

	resp, err := client.Get("https://api.example.com/v1/messages")
	if err != nil {
		t.Fatalf("an h2 client could not get through the terminated tunnel: %v", err)
	}

	defer resp.Body.Close()

	if resp.ProtoMajor != 2 {
		t.Fatalf("the tunnel answered %s: ALPN did not agree on h2, so the clients this feature "+
			"exists for are still untested against it", resp.Proto)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream says hello" {
		t.Errorf("the response did not come back through the tunnel: %q", body)
	}

	out := <-got

	if h := out.Header.Get("Authorization"); h != "Bearer sk-live-secret" {
		t.Errorf("the request left the tunnel with %q, want the real credential", h)
	}

	if out.ProtoMajor != 1 {
		t.Errorf("the request handed to the filter is %s: the relay writes HTTP/1.1 upstream",
			out.Proto)
	}

	if out.URL.Scheme != "https" || out.URL.Host != "api.example.com:443" {
		t.Errorf("the request handed on was %s://%s, want https://api.example.com:443",
			out.URL.Scheme, out.URL.Host)
	}
}

// A request for an authority the tunnel was not opened for is answered 421 and never sent.
//
// This is HTTP/2's doing. A client may coalesce requests for several authorities onto one
// connection it opened for a different one, so request two on an h2 stream can name a host nothing
// upstream of here was checked against. The authority used to be overwritten with the CONNECT
// target, which turned that disagreement into a request credentialled for a host the client never
// named. 421 is the status that says "wrong connection for this request": Go's own h2 client
// re-dials on it rather than failing, so a coalescing client recovers by itself.
func TestAnHTTP2RequestForAnotherAuthorityIs421(t *testing.T) {
	v := newVault(t, "api.example.com", "other.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	reached := make(chan string, 1)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.Host

		w.WriteHeader(http.StatusOK)
	})

	client := h2Through(t, Intercept(inner, v), v, "api.example.com:443")

	req, err := http.NewRequest(http.MethodGet, "https://api.example.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}

	// The connection is to api.example.com; the request says it is for somewhere else.
	req.Host = "other.example.com"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the request was not answered at all: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("an authority the tunnel was not opened for = %d, want 421", resp.StatusCode)
	}

	select {
	case host := <-reached:
		t.Errorf("the filter was handed the request anyway, as %q: nothing may be sent for a "+
			"destination the three names do not agree on", host)
	default:
	}
}

// The SNI is the third name, and it is checked rather than assumed. A handshake that asked for one
// host inside a tunnel opened for another is 421, and so is one that named no host at all - a
// vault whose leaves are issued for names has nothing to check that against.
func TestARequestWhoseTLSNameDoesNotMatchTheTunnelIs421(t *testing.T) {
	for name, cfg := range map[string]*tls.Config{
		// InsecureSkipVerify because a sandbox's real client would reject the leaf this gets
		// back: the certificate is minted for the CONNECT target, not for what it asked for.
		"another server name": {ServerName: "other.example.com", InsecureSkipVerify: true},
		"no server name":      {InsecureSkipVerify: true},
	} {
		v := newVault(t, "api.example.com", "other.example.com")

		if _, err := v.Create(bearerFor("api.example.com")); err != nil {
			t.Fatal(err)
		}

		reached := make(chan string, 1)

		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached <- r.Host

			w.WriteHeader(http.StatusOK)
		})

		client := tlsThrough(t, Intercept(inner, v), "api.example.com:443", cfg)

		// The authority agrees with the CONNECT target, so only the TLS name disagrees.
		fmt.Fprint(client, "GET /v1/messages HTTP/1.1\r\nHost: api.example.com\r\n"+
			"Connection: close\r\n\r\n")

		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatalf("%s: reading the response inside the tunnel: %v", name, err)
		}

		if resp.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("%s = %d, want 421", name, resp.StatusCode)
		}

		resp.Body.Close()

		select {
		case host := <-reached:
			t.Errorf("%s: the filter was handed the request anyway, as %q", name, host)
		default:
		}
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

// Every terminated tunnel gives its goroutine back.
//
// http.Serve returns only when Accept fails, and the listener serveTerminated hands it yields one
// connection and then blocks until it is closed. Nothing closed it, so http.Serve never returned,
// serveTerminated never returned, and one goroutine sat parked per terminated tunnel for the life
// of the daemon. Counted rather than reasoned about: a leak is a number that does not come back
// down.
func TestATerminatedTunnelGivesItsGoroutineBack(t *testing.T) {
	const tunnels = 20

	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream says hello")
	})

	srv := httptest.NewServer(Intercept(inner, v))
	defer srv.Close()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(v.CA().PEM()) {
		t.Fatal("the vault's authority PEM did not parse: a sandbox could never trust it")
	}

	before := serving()

	for range tunnels {
		oneTerminatedTunnel(t, srv.URL, pool)
	}

	// Given a moment to come down rather than read once: the daemon's side of the last tunnel
	// finishes on its own goroutine, after the client has its response.
	deadline := time.Now().Add(10 * time.Second)

	for {
		got := serving()
		if got <= before {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("%d of %d terminated tunnels are still inside serveTerminated (%d were "+
				"before they ran): each one parks a goroutine for the life of the daemon",
				got-before, tunnels, before)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// oneTerminatedTunnel is a whole tunnel from the sandbox's side: CONNECT, handshake, one request,
// and a close. The daemon's half must be finished by the time this returns, or soon after.
func oneTerminatedTunnel(t *testing.T, proxy string, pool *x509.CertPool) {
	t.Helper()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()

	fmt.Fprint(conn, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n")

	br := bufio.NewReader(conn)

	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT got %d", resp.StatusCode)
	}

	client := tls.Client(conn, &tls.Config{ServerName: "api.example.com", RootCAs: pool})
	if err := client.Handshake(); err != nil {
		t.Fatalf("the sandbox's client would not complete the handshake: %v", err)
	}

	defer client.Close()

	fmt.Fprint(client, "GET /v1/models HTTP/1.1\r\nHost: api.example.com\r\nConnection: close\r\n\r\n")

	inside, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("reading the response inside the tunnel: %v", err)
	}

	defer inside.Body.Close()

	_, _ = io.Copy(io.Discard, inside.Body)
}

// serving counts the goroutines still inside serveTerminated, which is one per tunnel that has
// not finished. A stack dump rather than runtime.NumGoroutine: the test server has goroutines of
// its own, and a count that includes them would be a count that cannot be compared.
func serving() int {
	buf := make([]byte, 1<<16)

	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte("credvault.serveTerminated"))
		}

		buf = make([]byte, 2*len(buf))
	}
}
