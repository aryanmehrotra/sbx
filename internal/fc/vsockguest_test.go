package fc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/execdctl"
)

// fakeDevice is the host half of a Firecracker hybrid-vsock device with an HTTP server behind
// one guest port: CONNECT <port> for that port is answered OK and the stream is handed to h;
// any other port is hung up on, which is what Firecracker does when nothing in the guest listens.
type fakeDevice struct {
	path string

	mu    sync.Mutex
	ports []string
}

func newFakeDevice(t *testing.T, port int, h http.Handler) *fakeDevice {
	t.Helper()

	// sun_path is 104 bytes on darwin; t.TempDir() can come close enough to fail for a reason
	// that has nothing to do with vsock.
	dir, err := os.MkdirTemp("", "fcg")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return newFakeDeviceAt(t, filepath.Join(dir, VsockName), port, h)
}

// newFakeDeviceAt is newFakeDevice bound at path, as a jailed VMM binds its device in its root.
func newFakeDeviceAt(t *testing.T, path string, port int, h http.Handler) *fakeDevice {
	t.Helper()

	d := &fakeDevice{path: path}

	ln, err := net.Listen("unix", d.path)
	if err != nil {
		t.Fatal(err)
	}

	conns := make(chan net.Conn)
	srv := &http.Server{Handler: h}

	go func() { _ = srv.Serve(&chanListener{ch: conns, addr: ln.Addr()}) }()

	t.Cleanup(func() { _ = ln.Close(); _ = srv.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go func() {
				line, err := bufio.NewReader(io.LimitReader(c, 64)).ReadString('\n')
				if err != nil {
					_ = c.Close()
					return
				}

				got := strings.TrimSpace(strings.TrimPrefix(line, "CONNECT "))

				d.mu.Lock()
				d.ports = append(d.ports, got)
				d.mu.Unlock()

				if got != strconv.Itoa(port) {
					_ = c.Close()
					return
				}

				_, _ = io.WriteString(c, "OK 1073741824\n")
				conns <- c
			}()
		}
	}()

	return d
}

type chanListener struct {
	ch   chan net.Conn
	addr net.Addr
	once sync.Once
	done chan struct{}
}

func (l *chanListener) Accept() (net.Conn, error) {
	l.once.Do(func() { l.done = make(chan struct{}) })

	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { l.done = make(chan struct{}) })

	select {
	case <-l.done:
	default:
		close(l.done)
	}

	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

type recorded struct {
	path, secret string
	body         execdctl.Rekey
}

func TestVsockGuestSealsAndRekeysOverTheDevice(t *testing.T) {
	var (
		mu  sync.Mutex
		got []recorded
	)

	mux := http.NewServeMux()
	for _, p := range []string{execdctl.PathSeal, execdctl.PathRekey} {
		mux.HandleFunc("POST "+p, func(w http.ResponseWriter, r *http.Request) {
			rec := recorded{path: r.URL.Path, secret: r.Header.Get(execdctl.SecretHeader)}
			_ = json.NewDecoder(r.Body).Decode(&rec.body)

			mu.Lock()
			got = append(got, rec)
			mu.Unlock()

			w.WriteHeader(http.StatusNoContent)
		})
	}

	dev := newFakeDevice(t, ExecdVsockPort, mux)
	vm := GuestVM{Sandbox: "s", Service: "web", VsockUDS: dev.path}

	var g Guest = VsockGuest{}
	if !g.Available() {
		t.Fatal("VsockGuest must report itself available: it is the real channel")
	}

	ctx := context.Background()

	if err := g.Seal(ctx, vm, "boot-secret"); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	err := g.Rekey(ctx, vm, Rekey{
		Secret: "boot-secret", Generation: 7, AccessToken: "tok",
		ControlSecret: "next-secret", Env: []string{"A=1", "B=two=2"},
	})
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("execd saw %d control calls, want 2: %+v", len(got), got)
	}

	if got[0].path != execdctl.PathSeal || got[0].secret != "boot-secret" {
		t.Errorf("seal = %+v, want %s authorised by the boot secret", got[0], execdctl.PathSeal)
	}

	r := got[1]
	if r.path != execdctl.PathRekey || r.secret != "boot-secret" {
		t.Errorf("rekey = %+v, want %s authorised by the CURRENT secret, not the next one", r, execdctl.PathRekey)
	}

	if r.body.Generation != 7 || r.body.AccessToken != "tok" || r.body.ControlSecret != "next-secret" {
		t.Errorf("rekey body = %+v", r.body)
	}

	if r.body.Envs["A"] != "1" || r.body.Envs["B"] != "two=2" || len(r.body.Envs) != 2 {
		t.Errorf("env = %v, want A=1 and B=two=2 split on the first '='", r.body.Envs)
	}

	if ports := dev.ports; len(ports) != 2 || ports[0] != strconv.Itoa(ExecdVsockPort) {
		t.Errorf("CONNECT lines = %v, want execd's port %d each time", ports, ExecdVsockPort)
	}
}

func TestVsockGuestRekeyWithoutEnvLeavesItAlone(t *testing.T) {
	var raw map[string]json.RawMessage

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+execdctl.PathRekey, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.WriteHeader(http.StatusNoContent)
	})

	dev := newFakeDevice(t, ExecdVsockPort, mux)

	err := VsockGuest{}.Rekey(context.Background(), GuestVM{VsockUDS: dev.path},
		Rekey{Secret: "a", Generation: 1, AccessToken: "t", ControlSecret: "b"})
	if err != nil {
		t.Fatal(err)
	}

	// nil Envs is "leave the env as it is"; an empty map would clear every key a claim set.
	if string(raw["envs"]) != "null" {
		t.Errorf("envs = %s, want null", raw["envs"])
	}
}

func TestVsockGuestRefusalCarriesExecdCode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+execdctl.PathRekey, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"STALE_GENERATION","message":"generation 1 is not after 4"}`)
	})

	dev := newFakeDevice(t, ExecdVsockPort, mux)

	err := VsockGuest{}.Rekey(context.Background(), GuestVM{VsockUDS: dev.path},
		Rekey{Secret: "a", Generation: 1, AccessToken: "t", ControlSecret: "b"})

	var e *execdctl.Error
	if !errors.As(err, &e) || e.Code != execdctl.CodeStaleGeneration {
		t.Fatalf("err = %v, want an execdctl.Error with %s", err, execdctl.CodeStaleGeneration)
	}
}

func TestVsockGuestDialReachesAnyGuestPort(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pong") })

	dev := newFakeDevice(t, 8080, mux)

	c, err := VsockGuest{}.Dial(context.Background(), GuestVM{VsockUDS: dev.path}, 8080)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _ = io.WriteString(c, "GET /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}

	b, _ := io.ReadAll(resp.Body)
	if string(b) != "pong" {
		t.Errorf("body = %q", b)
	}

	// A port nothing listens on is the device hanging up: a refusal, not a hang.
	if _, err := (VsockGuest{}).Dial(context.Background(), GuestVM{VsockUDS: dev.path}, 9); err == nil {
		t.Error("dial to a port with no listener succeeded")
	}
}

func TestVsockGuestRefusesOutOfRangePort(t *testing.T) {
	for _, p := range []int{0, -1} {
		if _, err := (VsockGuest{}).Dial(context.Background(), GuestVM{VsockUDS: "/nonexistent"}, p); err == nil {
			t.Errorf("port %d was accepted", p)
		}
	}
}
