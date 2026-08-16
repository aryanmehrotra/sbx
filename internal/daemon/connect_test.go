package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"strings"
	"testing"
	"time"
)

const testToken = "s3cr3t-token"

// echoServer stands in for a sandbox's wake port: whatever it is sent, it sends back.
func echoPort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go func() {
				defer c.Close()

				buf := make([]byte, 4096)

				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, err := c.Write(buf[:n]); err != nil {
							return
						}
					}

					if err != nil {
						return
					}
				}
			}()
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port
}

// daemonFronting builds a daemon that claims to front one port, as discovery would have.
func daemonFronting(port int, instance string) *daemon {
	d := New(nil, time.Minute, time.Minute, time.Minute)
	u := newUnit("demo", "db", "sbx-demo-db", instance, "demo/db",
		[]leg{{Listen: port, Upstream: provider.Endpoint{Host: "127.0.0.1", Port: port}}}, true)
	d.units["sbx-demo-db"] = u

	return d
}

func serverFor(t *testing.T, d *daemon) *httptest.Server {
	t.Helper()

	srv, err := d.Connect(ConnectOptions{Addr: "127.0.0.1:0", Token: testToken})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)

	return ts
}

// --- auth ----------------------------------------------------------------------------------

// The token is the only thing between the open internet and every sandbox in the deployment.
func TestAuthRejectsEverythingButTheRightHeader(t *testing.T) {
	ts := serverFor(t, daemonFronting(1, "i1"))

	for name, req := range map[string]func() *http.Request{
		"no header": func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/fleet", nil)
			return r
		},
		"wrong token": func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/fleet", nil)
			r.Header.Set("Authorization", "Bearer nope")
			return r
		},
		"empty bearer": func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/fleet", nil)
			r.Header.Set("Authorization", "Bearer ")
			return r
		},
		"not a bearer": func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/fleet", nil)
			r.Header.Set("Authorization", testToken)
			return r
		},
	} {
		resp, err := http.DefaultClient.Do(req())
		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s got %d, want 401", name, resp.StatusCode)
		}
	}
}

// A browser cannot set a custom header on a WebSocket handshake, but it can put anything in a
// URL. Accepting the token there would be the CSRF hole the header design exists to close.
func TestATokenInTheQueryStringIsRefused(t *testing.T) {
	ts := serverFor(t, daemonFronting(1, "i1"))

	resp, err := http.Get(ts.URL + "/v1/fleet?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a token in the URL got %d, want 400", resp.StatusCode)
	}
}

// The probe carries no credential, by design: a platform's health check has none to give.
func TestHealthzNeedsNoToken(t *testing.T) {
	ts := serverFor(t, daemonFronting(1, "i1"))

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz got %d, want 200", resp.StatusCode)
	}
}

// --- refusing to start ---------------------------------------------------------------------

func TestConnectRefusesToStartUnsafely(t *testing.T) {
	d := daemonFronting(1, "i1")

	if _, err := d.Connect(ConnectOptions{Addr: "127.0.0.1:0", Token: ""}); err == nil {
		t.Error("an empty token was accepted; the endpoint would be open to anyone who found it")
	}

	if _, err := d.Connect(ConnectOptions{Addr: ":8080", Token: testToken}); err == nil {
		t.Error("a public address with no TLS claim was accepted")
	}

	if _, err := d.Connect(ConnectOptions{Addr: ":8080", Token: testToken, BehindProxy: true}); err != nil {
		t.Errorf("a public address behind a stated terminator was refused: %v", err)
	}
}

// --- the allow-list and the instance check ---------------------------------------------------

// Without this the handler is an open proxy into whatever the deployment can reach, which is a
// far larger thing than a tunnel to a sandbox.
func TestAPortThisDaemonDoesNotFrontIsNeverDialled(t *testing.T) {
	victim := echoPort(t) // stands in for something else on the box
	ts := serverFor(t, daemonFronting(victim+1, "i1"))

	code, _ := rawDial(t, ts.URL, victim, "i1")
	if code != http.StatusForbidden {
		t.Errorf("a port the daemon does not front got %d, want 403", code)
	}
}

// A docker ref is a name, and `sbx rm x && sbx create x` reuses it on the same port with a new
// empty volume. Addressing by port alone would splice a client into that and report success.
func TestAPortWhoseInstanceChangedIsRefused(t *testing.T) {
	port := echoPort(t)
	ts := serverFor(t, daemonFronting(port, "instance-two"))

	code, _ := rawDial(t, ts.URL, port, "instance-one")
	if code != http.StatusConflict {
		t.Errorf("a stale instance got %d, want 409", code)
	}

	// And a client that names nothing cannot be told it is wrong, so it is refused too.
	if code, _ := rawDial(t, ts.URL, port, ""); code != http.StatusConflict {
		t.Errorf("a dial naming no instance got %d, want 409", code)
	}
}

// --- end to end ------------------------------------------------------------------------------

func TestBytesSurviveTheTunnel(t *testing.T) {
	port := echoPort(t)
	ts := serverFor(t, daemonFronting(port, "i1"))

	roundTrip(t, ts.URL, port, "i1")
}

// The claim the whole transport choice rests on: a layer 7 proxy terminates TLS and strips
// ALPN, and WebSocket is what survives it. A test that only ever talks to httptest never
// checks that claim, so this puts a real reverse proxy in the middle.
func TestBytesSurviveALayer7ProxyInFront(t *testing.T) {
	port := echoPort(t)
	ts := serverFor(t, daemonFronting(port, "i1"))

	target, _ := url.Parse(ts.URL)
	front := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))

	t.Cleanup(front.Close)

	roundTrip(t, front.URL, port, "i1")
}

// --- helpers ---------------------------------------------------------------------------------

// rawDial performs the WebSocket handshake by hand and returns the status and the connection.
// By hand because the point is to test our own handshake, not a library's.
func rawDial(t *testing.T, base string, port int, instance string) (int, *wsConn) {
	t.Helper()

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}

	req := fmt.Sprintf("GET /v1/connect?port=%d&instance=%s HTTP/1.1\r\n"+
		"Host: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n"+
		"Authorization: Bearer %s\r\n\r\n", port, instance, u.Host, wsKey(), testToken)

	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(conn)

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()

		return resp.StatusCode, nil
	}

	t.Cleanup(func() { _ = conn.Close() })

	return resp.StatusCode, &wsConn{conn: conn, br: br, mask: true, lastPong: time.Now()}
}

func roundTrip(t *testing.T, base string, port int, instance string) {
	t.Helper()

	code, ws := rawDial(t, base, port, instance)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("handshake got %d, want 101", code)
	}

	// Small payload, then one larger than a single relay chunk, because the second is where a
	// length-form or reassembly bug shows up.
	for _, payload := range [][]byte{[]byte("hello"), bytes.Repeat([]byte("x"), 100<<10)} {
		if err := ws.write(opBinary, payload); err != nil {
			t.Fatalf("write of %d bytes: %v", len(payload), err)
		}

		var got []byte

		deadline := time.Now().Add(10 * time.Second)

		for len(got) < len(payload) && time.Now().Before(deadline) {
			_ = ws.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

			chunk, err := ws.readServerFrame()
			if err != nil {
				t.Fatalf("read after %d of %d bytes: %v", len(got), len(payload), err)
			}

			got = append(got, chunk...)
		}

		if !bytes.Equal(got, payload) {
			t.Fatalf("echoed %d bytes, sent %d", len(got), len(payload))
		}
	}
}

func fleetOf(t *testing.T, ts *httptest.Server) []fleetService {
	t.Helper()

	r, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/fleet", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	var body struct {
		Services []fleetService `json:"services"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	return body.Services
}

// The client needs the fleet to know which listeners to open, and the instance to dial with.
func TestFleetCarriesWhatTheClientNeeds(t *testing.T) {
	port := echoPort(t)
	ts := serverFor(t, daemonFronting(port, "i-abc"))

	got := fleetOf(t, ts)
	if len(got) != 1 {
		t.Fatalf("fleet has %d services, want 1", len(got))
	}

	if got[0].Instance != "i-abc" {
		t.Errorf("fleet instance = %q, want i-abc - without it the client cannot dial", got[0].Instance)
	}

	if len(got[0].Ports) != 1 || got[0].Ports[0] != port {
		t.Errorf("fleet ports = %v, want [%d]", got[0].Ports, port)
	}

	if strings.TrimSpace(got[0].Sandbox) == "" {
		t.Error("fleet does not say which sandbox a service belongs to")
	}
}

// A deployed daemon that cannot reach a container runtime must stay up and say so.
//
// Exiting is right on a laptop and wrong once deployed: the process vanishes, the platform's
// scale-to-zero never completes a wake, and the operator gets a holding page forever with
// nothing to read. Measured on zopcloud before this existed - four minutes of "Starting up…".
func TestADaemonWithNoRuntimeStaysUpAndExplains(t *testing.T) {
	d := daemonFronting(1, "i1")
	d.startupErr = errors.New("no docker daemon found on linux: set DOCKER_HOST, or pass --socket")

	ts := serverFor(t, d)

	// Liveness answers, because the process IS alive. A failing probe here would have the
	// platform restart something a restart cannot fix, and the loop hides the reason.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz got %d with no runtime, want 200 - liveness is not readiness", resp.StatusCode)
	}

	if !strings.Contains(string(body), "degraded") || !strings.Contains(string(body), "DOCKER_HOST") {
		t.Errorf("healthz body = %q, want it to say degraded and carry the reason", body)
	}

	// The fleet must not answer "no sandboxes", which is a different claim from "I cannot see
	// any sandboxes" and is the one a client would report.
	r, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/fleet", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)

	fr, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	fb, _ := io.ReadAll(fr.Body)
	_ = fr.Body.Close()

	if fr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("fleet got %d with no runtime, want 503 rather than an empty list", fr.StatusCode)
	}

	if !strings.Contains(string(fb), "docker socket") {
		t.Errorf("fleet body = %q, want it to say what would fix this", fb)
	}
}
