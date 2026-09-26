package provider

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// fakeEngine answers the two Engine API calls a Start makes - inspect and start - for one
// container, and records whether the start was ever sent.
type fakeEngine struct {
	mu     sync.Mutex
	labels map[string]string
	starts int
}

func (f *fakeEngine) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": "c1", "Name": "/sbx-osb-a-sandbox",
			"State":  map[string]any{"Status": "exited"},
			"Config": map[string]any{"Labels": f.labels},
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
		f.starts++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func dockerAgainst(t *testing.T, f *fakeEngine) *dockerProvider {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	addr := srv.Listener.Addr().String()

	return &dockerProvider{api: &dockerClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}}}
}

// Docker resolves a bind source again on every container start. The API checked a host volume
// at create; a symlink swapped in while the sandbox was stopped would redirect the mount at the
// next wake - to anywhere the engine can read. A start must mount exactly what create
// validated, so it is refused, and docker is never asked.
func TestStartRefusesAHostBindSwappedForASymlink(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r // /var -> /private/var on a Mac: the API pins the canonical path
	}

	work := filepath.Join(root, "work")
	outside := t.TempDir()

	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}

	label := hostBindsLabel([]spec.VolumeMount{{Host: work, Target: "/mnt/work"}, {Volume: "v", Target: "/v"}})
	if label == "" {
		t.Fatal("no pin recorded for a host volume")
	}

	f := &fakeEngine{labels: map[string]string{labelHostBinds: label}}
	d := dockerAgainst(t, f)

	if err := d.Start(context.Background(), "sbx-osb-a-sandbox"); err != nil {
		t.Fatalf("start with the host volume unchanged: %v", err)
	}

	// The swap: the directory create checked becomes a link to somewhere else.
	if err := os.Rename(work, work+".orig"); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outside, work); err != nil {
		t.Fatal(err)
	}

	err := d.Start(context.Background(), "sbx-osb-a-sandbox")
	if err == nil || !strings.Contains(err.Error(), work) {
		t.Fatalf("start after the swap = %v, want a refusal naming %s", err, work)
	}

	f.mu.Lock()
	starts := f.starts
	f.mu.Unlock()

	if starts != 1 {
		t.Errorf("docker was asked to start %d times, want 1: the swapped start must never reach it", starts)
	}

	// Gone altogether is refused too, rather than handed to docker.
	_ = os.Remove(work)

	if err := d.Start(context.Background(), "sbx-osb-a-sandbox"); err == nil {
		t.Error("start with the host volume deleted was allowed")
	}
}

// A container with no pinned binds - every sandbox.json container - starts as it always did,
// without the check reading anything.
func TestStartWithoutPinnedBindsIsUnchanged(t *testing.T) {
	f := &fakeEngine{labels: map[string]string{labelSandbox: "x"}}
	d := dockerAgainst(t, f)

	if err := d.Start(context.Background(), "sbx-x-db"); err != nil {
		t.Fatal(err)
	}

	if f.starts != 1 {
		t.Errorf("starts = %d, want 1", f.starts)
	}

	if hostBindsLabel([]spec.VolumeMount{{Volume: "v", Target: "/v"}}) != "" {
		t.Error("a named volume was pinned as a host bind")
	}
}
