package fchost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/osb"
)

func env(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}

	return func(k string) string { return m[k] }
}

func dirOf(d string) func() (string, error) { return func() (string, error) { return d, nil } }

// `sbx serve --provider firecracker --osb-addr` on a helper-VM host keys the API exactly as Linux
// does: --osb-key, else SBX_OSB_KEY, else a key generated once into ~/.sbx/osb/key on THIS machine
// (0600), where `sbx mcp` and the SDK examples look. That key is what the in-VM daemon is given.
func TestServeOnTheHelperVMGeneratesTheKeyHereAsLinuxDoes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "osb")

	opt, err := parseServe([]string{"--osb-addr", "127.0.0.1:18080"}, env(), dirOf(dir))
	if err != nil {
		t.Fatalf("parseServe --osb-addr: %v", err)
	}

	stored := osb.ReadKey(dir)
	if stored == "" || opt.OSBKey != stored || opt.OSBAddr != "127.0.0.1:18080" {
		t.Fatalf("key %q, stored %q, addr %q", opt.OSBKey, stored, opt.OSBAddr)
	}

	if fi, err := os.Stat(osb.KeyFile(dir)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}

	if !strings.Contains(opt.keyNote, osb.KeyFile(dir)) || strings.Contains(opt.keyNote, stored) {
		t.Fatalf("the startup line must name where the key is and never the key: %q", opt.keyNote)
	}

	again, err := parseServe([]string{"--osb-addr", "127.0.0.1:18080"}, env(), dirOf(dir))
	if err != nil || again.OSBKey != opt.OSBKey {
		t.Fatalf("a restart must keep the key: %q vs %q (%v)", again.OSBKey, opt.OSBKey, err)
	}

	for _, tc := range []struct {
		args []string
		env  func(string) string
		want string
	}{
		{[]string{"--osb-addr", "127.0.0.1:18080", "--osb-key", "flagkey"}, env("SBX_OSB_KEY", "envkey"), "flagkey"},
		{[]string{"--osb-addr", "127.0.0.1:18080"}, env("SBX_OSB_KEY", "envkey"), "envkey"},
	} {
		empty := filepath.Join(t.TempDir(), "osb")

		opt, err := parseServe(tc.args, tc.env, dirOf(empty))
		if err != nil || opt.OSBKey != tc.want {
			t.Errorf("%v: key %q, want %q (%v)", tc.args, opt.OSBKey, tc.want, err)
		}

		if _, err := os.Stat(osb.KeyFile(empty)); err == nil {
			t.Errorf("%v: generated a key although one was given", tc.args)
		}
	}
}

// Without --osb-addr nothing is generated: a key nobody asked for is a file nobody expects.
func TestServeOnTheHelperVMWithoutTheAPIMakesNoKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "osb")

	opt, err := parseServe([]string{"--idle", "2m"}, env(), dirOf(dir))
	if err != nil || opt.OSBAddr != "" || opt.OSBKey != "" {
		t.Fatalf("%+v %v", opt, err)
	}

	if _, err := os.Stat(osb.KeyFile(dir)); err == nil {
		t.Fatal("generated a key for a daemon that serves no API")
	}
}

// The v0.13 fail-closed switches reach the VM's daemon, by name; the unsafe ones never by default.
func TestServeOnTheHelperVMPassesTheJailerOptOutOnlyWhenTyped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "osb")

	opt, err := parseServe([]string{"--osb-addr", "127.0.0.1:18080"}, env(), dirOf(dir))
	if err != nil {
		t.Fatal(err)
	}

	if slices.Contains(opt.Serve, "--osb-insecure-no-jailer") {
		t.Fatalf("the jailer opt-out was passed without being asked for: %q", opt.Serve)
	}

	opt, err = parseServe([]string{"--osb-addr", "127.0.0.1:18080", "--osb-insecure-no-jailer"}, env(), dirOf(dir))
	if err != nil || !slices.Contains(opt.Serve, "--osb-insecure-no-jailer") {
		t.Fatalf("--osb-insecure-no-jailer not passed on: %q %v", opt.Serve, err)
	}
}

// What the helper-VM path cannot honour is refused by name, never dropped: a keyless API (the ssh
// forward is on this machine's loopback, which containers on a VM-backed engine reach), and the
// warm pool (not carried into the VM yet).
func TestServeOnTheHelperVMRefusesWhatItCannotHonour(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  func(string) string
		want string
	}{
		{[]string{"--osb-addr", "127.0.0.1:18080", "--osb-insecure-no-key"}, env(), "--osb-insecure-no-key"},
		{[]string{"--osb-addr", "127.0.0.1:18080", "--osb-pool", "python:3.11-slim=2"}, env(), "--osb-pool"},
		{[]string{"--osb-addr", "127.0.0.1:18080", "--osb-pool-freeze"}, env(), "--osb-pool"},
		{[]string{"--osb-addr", "127.0.0.1:18080"}, env("SBX_OSB_POOL", "python:3.11-slim"), "SBX_OSB_POOL"},
	} {
		dir := filepath.Join(t.TempDir(), "osb")

		_, err := parseServe(tc.args, tc.env, dirOf(dir))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "helper VM") {
			t.Errorf("%v: got %v, want a refusal naming %s", tc.args, err, tc.want)
		}
	}
}

// keyedAPI is the in-VM OpenSandbox API as the host sees it through the tunnel: /health open,
// everything else behind key, as osb.Server.authed.
func keyedAPI(t *testing.T, key string, next http.HandlerFunc) *httptest.Server {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}

		if key != "" && r.Header.Get("OPEN-SANDBOX-API-KEY") != key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if next != nil {
			next(w, r)
			return
		}

		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	t.Cleanup(ts.Close)

	return ts
}

func frontErr(t *testing.T, key string, api *httptest.Server) error {
	t.Helper()

	m := limaManager(t, &fakeRunner{})

	tok, _ := m.Token()
	vm := fakeVMDaemon(t, tok, 45434)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return m.Front(ctx, FrontOptions{
		EnsureOptions: EnsureOptions{OSBKey: key},
		OSBAddr:       fmt.Sprintf("127.0.0.1:%d", freeLocal(t)),
		Refresh:       50 * time.Millisecond, Shift: freeLocal(t) - 45434, skipEnsure: true,
		tunnel: func(ctx context.Context) (Endpoints, func() error, error) {
			return Endpoints{Connect: vm.URL, OSB: api.URL}, func() error { <-ctx.Done(); return nil }, nil
		},
	})
}

// Loopback is not a trust boundary on a VM-backed engine, and the ssh forward of the in-VM API is
// a loopback port on this machine. So the front proves, before it serves, that the API behind it
// refuses a request without the key and accepts this one - and refuses to serve otherwise.
func TestFrontRefusesAnAPIThatAnswersWithoutTheKey(t *testing.T) {
	if err := frontErr(t, "k", keyedAPI(t, "", nil)); err == nil || !strings.Contains(err.Error(), "without the key") {
		t.Fatalf("an in-VM API that answers keyless was fronted: %v", err)
	}
}

func TestFrontRefusesAnAPIThatRejectsItsKey(t *testing.T) {
	if err := frontErr(t, "k", keyedAPI(t, "some-other-key", nil)); err == nil || !strings.Contains(err.Error(), "rejects the key") {
		t.Fatalf("an in-VM API holding another key was fronted: %v", err)
	}
}

func TestFrontRefusesTheAPIWithNoKey(t *testing.T) {
	if err := frontErr(t, "", keyedAPI(t, "", nil)); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("the API was fronted with no key at all: %v", err)
	}
}

// An SDK dials an endpoint the moment create answers. The mirror binds new ports on a tick, so a
// create answered before the tick left the endpoint refusing: the front syncs the mirror before it
// hands a create's (or an endpoint lookup's) answer back.
func TestFrontMirrorsANewSandboxBeforeItsCreateAnswers(t *testing.T) {
	m := limaManager(t, &fakeRunner{})

	tok, _ := m.Token()

	remote := 45435
	local := freeLocal(t)

	var created atomic.Bool

	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/v1/fleet":
			if r.Header.Get("Authorization") != "Bearer "+tok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			svcs := []map[string]any{}
			if created.Load() {
				svcs = append(svcs, map[string]any{
					"sandbox": "osb-000000000001", "service": "main", "ref": "osb-000000000001-main",
					"instance": "i1", "awake": true, "ports": []int{remote},
				})
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"services": svcs, "halfClose": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(vm.Close)

	api := keyedAPI(t, "k", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes" {
			created.Store(true)
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"id":"osb-000000000001"}`)

			return
		}

		_, _ = io.WriteString(w, `{"items":[]}`)
	})

	osbAddr := fmt.Sprintf("127.0.0.1:%d", freeLocal(t))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- m.Front(ctx, FrontOptions{
			EnsureOptions: EnsureOptions{OSBKey: "k"},
			// An hour: only the sync can bind the port in time.
			OSBAddr: osbAddr, Refresh: time.Hour, Shift: local - remote, skipEnsure: true,
			tunnel: func(ctx context.Context) (Endpoints, func() error, error) {
				return Endpoints{Connect: vm.URL, OSB: api.URL}, func() error { <-ctx.Done(); return nil }, nil
			},
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !up(mustPort(t, osbAddr)) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	req, _ := http.NewRequest(http.MethodPost, "http://"+osbAddr+"/v1/sandboxes", strings.NewReader(`{}`))
	req.Header.Set("OPEN-SANDBOX-API-KEY", "k")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create through the front: %d", resp.StatusCode)
	}

	if !up(local) {
		t.Fatalf("create answered before its port %d was mirrored at %d", remote, local)
	}

	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Front after cancel: %v", err)
	}
}

func mustPort(t *testing.T, addr string) int {
	t.Helper()

	var p int
	if _, err := fmt.Sscanf(addr[strings.LastIndex(addr, ":")+1:], "%d", &p); err != nil {
		t.Fatal(err)
	}

	return p
}
