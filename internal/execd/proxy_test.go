//go:build unix

package execd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/wsclient"
	"github.com/aryanmehrotra/sbx/internal/wsclient/wstest"
)

// backend is a service "inside the sandbox" on a loopback port, returning that port.
func backend(t *testing.T, h http.HandlerFunc) string {
	t.Helper()

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)

	return u.Port()
}

func TestProxyHTTP(t *testing.T) {
	type seen struct {
		uri, host, token, xff string
		multi                 []string
		body                  string
	}

	got := make(chan seen, 1)

	port := backend(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{uri: r.RequestURI, host: r.Host, token: r.Header.Get(AccessTokenHeader),
			xff: r.Header.Get("X-Forwarded-For"), multi: r.Header.Values("X-Multi"), body: string(b)}

		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "from the service")
	})

	s := newTestServer(t, Options{AccessToken: "tok"})

	req, _ := http.NewRequest("POST", s.ts.URL+"/proxy/"+port+"/api/a%2Fb/c?x=1&x=2", strings.NewReader("payload"))
	req.Header.Set(AccessTokenHeader, "tok")
	req.Header.Add("X-Multi", "one")
	req.Header.Add("X-Multi", "two")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTeapot || string(body) != "from the service" {
		t.Fatalf("status %d body %q: the service's status and body must pass through", resp.StatusCode, body)
	}

	if c := resp.Header.Values("Set-Cookie"); len(c) != 2 {
		t.Fatalf("Set-Cookie %v: repeated response headers must survive", c)
	}

	sn := <-got

	if sn.uri != "/api/a%2Fb/c?x=1&x=2" {
		t.Errorf("service saw %q: prefix not stripped, or the path or query re-encoded", sn.uri)
	}

	if sn.token != "" {
		t.Error("the execd token was forwarded to the service")
	}

	if len(sn.multi) != 2 || sn.body != "payload" || sn.xff == "" {
		t.Errorf("service saw headers %v body %q xff %q", sn.multi, sn.body, sn.xff)
	}

	if !strings.HasPrefix(sn.host, "127.0.0.1:") {
		t.Errorf("Host %q, want the incoming host", sn.host)
	}
}

func TestProxyRootAndBarePort(t *testing.T) {
	port := backend(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.URL.Path) })
	s := newTestServer(t, Options{})

	for _, p := range []string{"/proxy/" + port, "/proxy/" + port + "/"} {
		status, _, body := s.do("GET", p, nil)
		if status != http.StatusOK || string(body) != "/" {
			t.Errorf("GET %s: %d %q, want the service root", p, status, body)
		}
	}

	// Paths the ServeMux would clean or redirect reach the service untouched.
	status, _, body := s.do("GET", "/proxy/"+port+"//double//slash", nil)
	if status != http.StatusOK || string(body) != "//double//slash" {
		t.Errorf("double slashes: %d %q", status, body)
	}
}

func TestProxyRefusals(t *testing.T) {
	s := newTestServer(t, Options{AccessToken: "tok"})

	status, _, body := s.do("GET", "/proxy/8080/", nil)
	wantError(t, status, body, http.StatusUnauthorized, codeUnauthorized)

	for _, p := range []string{"/proxy/", "/proxy/abc/", "/proxy/0/", "/proxy/70000/"} {
		status, _, body := s.do("GET", p, nil, AccessTokenHeader, "tok")
		wantError(t, status, body, http.StatusBadRequest, codeInvalidRequest)
	}

	// A port nobody listens on: bind one, then free it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	_, free, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()

	status, _, body = s.do("GET", "/proxy/"+free+"/", nil, AccessTokenHeader, "tok")
	wantError(t, status, body, http.StatusBadGateway, codeRuntimeError)
}

func TestProxyWebSocket(t *testing.T) {
	port := backend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			http.NotFound(w, r)
			return
		}

		c, err := wstest.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()

		for {
			_, p, err := c.ReadMessage()
			if err != nil {
				return
			}

			_ = c.WriteText(append([]byte("echo:"), p...))
		}
	})

	s := newTestServer(t, Options{AccessToken: "tok"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(s.ts.URL, "http") + "/proxy/" + port + "/ws"

	// Without the token the upgrade is refused before it reaches the service.
	if _, err := wsclient.Dial(ctx, wsURL, wsclient.Options{}); err == nil {
		t.Fatal("upgrade without the token succeeded")
	}

	c, err := wsclient.Dial(ctx, wsURL, wsclient.Options{Header: http.Header{AccessTokenHeader: {"tok"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for _, m := range []string{"hello", "again"} {
		if err := c.WriteText([]byte(m)); err != nil {
			t.Fatal(err)
		}

		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))

		_, p, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}

		if string(p) != "echo:"+m {
			t.Fatalf("got %q", p)
		}
	}

	var he *wsclient.HandshakeError
	if _, err := wsclient.Dial(ctx, "ws"+strings.TrimPrefix(s.ts.URL, "http")+"/proxy/"+port+"/nope",
		wsclient.Options{Header: http.Header{AccessTokenHeader: {"tok"}}}); !errors.As(err, &he) {
		t.Fatalf("a refused upgrade at the service should come back as its HTTP answer, got %v", err)
	}
}
