//go:build unix

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/execd"
	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/wsserver"
)

// dialingGuest is a guest channel whose Dial reaches a real listener - an execd, or a stand-in -
// and records which guest port each connection asked for.
type dialingGuest struct {
	fakeGuest

	addr string

	mu    sync.Mutex
	ports []int
}

func (g *dialingGuest) Dial(ctx context.Context, _ fc.GuestVM, port int) (net.Conn, error) {
	g.mu.Lock()
	g.ports = append(g.ports, port)
	g.mu.Unlock()

	var d net.Dialer

	return d.DialContext(ctx, "tcp", g.addr)
}

// serveGuest puts h behind a loopback listener and makes it the rig's guest channel.
func serveGuest(t *testing.T, r *rig, h http.Handler) *dialingGuest {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &http.Server{Handler: h}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	g := &dialingGuest{fakeGuest: fakeGuest{available: true, launcher: r.l}, addr: ln.Addr().String()}
	r.p.guest = g

	return g
}

// realExecd is the guest's execd, run here: the same server fc-init execs as PID 1, holding the
// VM's access token, so a call without the token the provider recorded is refused.
func realExecd(t *testing.T, r *rig, ref string) *dialingGuest {
	t.Helper()

	s, err := execd.New(execd.Options{AccessToken: r.vm(t, ref).AccessToken})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Close)

	return serveGuest(t, r, s)
}

func TestExecRunsThroughExecdOverTheGuestChannel(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "x1", redis)
	g := realExecd(t, r, ref)

	out, err := r.p.Exec(r.ctx, ref, []string{"sh", "-c", "echo out; echo err >&2"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Fatalf("output = %q, want stdout and stderr together, as docker exec gives them", out)
	}

	// argv, not a shell line: an argument with a space arrives as one argument.
	out, err = r.p.Exec(r.ctx, ref, []string{"printf", "%s|", "a b", "c"})
	if err != nil || out != "a b|c|" {
		t.Fatalf("argv exec = %q, %v", out, err)
	}

	_, err = r.p.Exec(r.ctx, ref, []string{"sh", "-c", "echo nope; exit 3"})
	if err == nil || !strings.Contains(err.Error(), "exit") || !strings.Contains(err.Error(), "3") ||
		!strings.Contains(err.Error(), "nope") {
		t.Fatalf("a failing command = %v, want its exit status and output", err)
	}

	for _, p := range g.ports {
		if p != fc.ExecdVsockPort {
			t.Fatalf("dialled guest port %d, want execd's %d", p, fc.ExecdVsockPort)
		}
	}
}

func TestExecWithTheWrongTokenIsRefused(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "x2", redis)

	s, err := execd.New(execd.Options{AccessToken: "somebody-else"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Close)
	serveGuest(t, r, s)

	if _, err := r.p.Exec(r.ctx, ref, []string{"true"}); err == nil {
		t.Fatal("execd with a different token ran the command")
	}
}

func TestCopyInAndOutOfTheVM(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "x3", redis)
	realExecd(t, r, ref)

	// execd runs on this machine in the test, so "inside" is a directory here.
	guest := t.TempDir()
	host := t.TempDir()

	src := filepath.Join(host, "in.txt")
	if err := os.WriteFile(src, []byte("hello vm"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Copy(r.ctx, ref, src, ":"+filepath.Join(guest, "sub", "in.txt")); err != nil {
		t.Fatalf("copy in: %v", err)
	}

	if b, _ := os.ReadFile(filepath.Join(guest, "sub", "in.txt")); string(b) != "hello vm" {
		t.Fatalf("copied in %q", b)
	}

	// Into an existing directory: the file keeps its name, as docker cp does.
	if err := r.p.Copy(r.ctx, ref, src, ":"+guest); err != nil {
		t.Fatalf("copy into a dir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(guest, "in.txt")); err != nil {
		t.Fatalf("copy into a directory: %v", err)
	}

	dst := filepath.Join(host, "out.txt")
	if err := r.p.Copy(r.ctx, ref, ":"+filepath.Join(guest, "sub", "in.txt"), dst); err != nil {
		t.Fatalf("copy out: %v", err)
	}

	if b, _ := os.ReadFile(dst); string(b) != "hello vm" {
		t.Fatalf("copied out %q", b)
	}

	if err := r.p.Copy(r.ctx, ref, ":"+filepath.Join(guest, "missing"), dst); err == nil {
		t.Fatal("copying a file that does not exist succeeded")
	}

	// A directory is refused by name rather than half-copied.
	if err := r.p.Copy(r.ctx, ref, ":"+guest, filepath.Join(host, "d")); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("copying a directory out = %v", err)
	}
}

func TestExecTTYSpeaksThePTYProtocol(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "x4", redis)
	token := r.vm(t, ref).AccessToken

	var (
		mu      sync.Mutex
		created map[string]any
		stdin   bytes.Buffer
		deleted bool
		query   string
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /pty", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-EXECD-ACCESS-TOKEN") != token {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}

		mu.Lock()
		_ = json.NewDecoder(req.Body).Decode(&created)
		mu.Unlock()

		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"session_id":"p1"}`)
	})
	mux.HandleFunc("DELETE /pty/p1", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		deleted = true
		mu.Unlock()
	})
	mux.HandleFunc("GET /pty/p1/ws", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-EXECD-ACCESS-TOKEN") != token {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}

		mu.Lock()
		query = req.URL.RawQuery
		mu.Unlock()

		c, err := wsserver.Upgrade(w, req, wsserver.Options{})
		if err != nil {
			return
		}
		defer c.Close()

		_ = c.WriteMessage(wsserver.TextMessage, []byte(`{"type":"connected","session_id":"p1","mode":"pipe"}`))

		// Echo one stdin frame back as stdout and one as stderr, then exit 3.
		_, data, err := c.ReadMessage()
		if err != nil || len(data) == 0 || data[0] != 0x00 {
			return
		}

		mu.Lock()
		stdin.Write(data[1:])
		mu.Unlock()

		_ = c.WriteMessage(wsserver.BinaryMessage, append([]byte{0x01}, data[1:]...))
		_ = c.WriteMessage(wsserver.BinaryMessage, append([]byte{0x02}, []byte("warn")...))
		_ = c.WriteMessage(wsserver.TextMessage, []byte(`{"type":"exit","exit_code":3}`))
	})

	serveGuest(t, r, mux)

	var out, errw bytes.Buffer

	err := r.p.execTTY(r.ctx, ref, []string{"sh", "-c", "it's"}, strings.NewReader("typed"), &out, &errw)

	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if got := created["command"]; got != `exec 'sh' '-c' 'it'"'"'s'` {
		t.Errorf("pty command = %v, want argv quoted for sh and exec'd", got)
	}

	if out.String() != "typed" || errw.String() != "warn" || stdin.String() != "typed" {
		t.Errorf("stdout %q stderr %q stdin %q", out.String(), errw.String(), stdin.String())
	}

	// Not a terminal here, so pipe mode: stdout and stderr stay apart.
	if !strings.Contains(query, "pty=0") {
		t.Errorf("ws query = %q, want pty=0 for a non-terminal", query)
	}

	if !deleted {
		t.Error("the pty session was left behind in execd")
	}
}

func TestGuestDialerCoversExecdOnlyAndKeepsTheTapForTheWorkload(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "x5", redis)
	g := realExecd(t, r, ref)

	var gd GuestDialer = r.p

	if _, ok := gd.GuestDialer("x5", "cache", 6379); ok {
		t.Fatal("the workload's TCP port has no vsock listener; it must stay on the tap (ok=false)")
	}

	dial, ok := gd.GuestDialer("x5", "cache", fc.ExecdVsockPort)
	if !ok || dial == nil {
		t.Fatal("execd's port must be dialled over vsock")
	}

	c, err := dial(r.ctx)
	if err != nil {
		t.Fatal(err)
	}

	_ = c.Close()

	if len(g.ports) != 1 || g.ports[0] != fc.ExecdVsockPort {
		t.Fatalf("dialled %v", g.ports)
	}

	r.p.guest = fc.NoGuest{}
	if _, ok := r.p.GuestDialer("x5", "cache", fc.ExecdVsockPort); ok {
		t.Fatal("without a guest channel there is nothing to dial over vsock")
	}
}
