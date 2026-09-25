package osb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// fakeDocker is a provider with the capabilities the API needs, backed by a map. It records what
// it was asked to create, so a test can check the container the API asked for rather than the
// response it printed.
type fakeDocker struct {
	mu      sync.Mutex
	units   map[string]*provider.Unit
	created map[string]spec.Service
	removed []string
	paused  []string
	slot    int

	exitOnStart bool // new containers are not running: the entrypoint exited
	logs        string
	arch        string

	// Snapshots and named volumes.
	images     map[string]bool
	commits    []string // "ref -> image"
	changes    []string // docker commit --change values, all commits
	commitErr  error
	commitGate chan struct{} // when set, Commit waits for it to close
	removedImg []string
	pulls      []string
	volumes    map[string]bool
	volCreated []string
	volRemoved []string
	volErr     error
	inspectErr map[string]error // ImageInfo fails for these images
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{units: map[string]*provider.Unit{}, created: map[string]spec.Service{},
		logs: "hello from the container\n", arch: "arm64"}
}

func (f *fakeDocker) Name() string { return "fake" }

func (f *fakeDocker) Create(_ context.Context, sandbox string, slot, _ int, svc string, s spec.Service,
	eps []provider.Endpoint, _ string, _ provider.Isolation) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.created[sandbox] = s
	u := &provider.Unit{Sandbox: sandbox, Service: svc, Slot: slot, Ref: "sbx-" + sandbox + "-" + svc,
		Running: !f.exitOnStart, Client: eps, OnIdle: s.OnIdle}

	for _, e := range eps {
		u.Listen = append(u.Listen, e.Port)
		u.Upstream = append(u.Upstream, provider.Endpoint{Host: "127.0.0.1", Port: e.Port + 10000})
	}

	f.units[sandbox] = u

	return nil
}

func (f *fakeDocker) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []provider.Unit

	for sb, u := range f.units {
		if sandbox == "" || sb == sandbox {
			out = append(out, *u)
		}
	}

	return out, nil
}

func (f *fakeDocker) Remove(_ context.Context, sandbox string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.units[sandbox]; !ok {
		return fmt.Errorf("no sandbox %q", sandbox)
	}

	delete(f.units, sandbox)
	f.removed = append(f.removed, sandbox)

	return nil
}

func (f *fakeDocker) Endpoints(_, _ string, slot, start int, ports []int) []provider.Endpoint {
	var out []provider.Endpoint
	for i := range ports {
		out = append(out, provider.Endpoint{Host: "127.0.0.1", Port: 20000 + slot*20 + start + i})
	}

	return out
}

func (f *fakeDocker) AllocSlot(context.Context, string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.slot++

	return f.slot, nil
}

func (f *fakeDocker) Logs(_ context.Context, _ string, _ int, _ bool, w io.Writer) error {
	_, err := io.WriteString(w, f.logs)
	return err
}

func (f *fakeDocker) Start(context.Context, string) error                            { return nil }
func (f *fakeDocker) Stop(context.Context, string) error                             { return nil }
func (f *fakeDocker) Healthy(context.Context, string) (bool, bool)                   { return true, true }
func (f *fakeDocker) Probe(context.Context, string) (bool, bool)                     { return true, true }
func (f *fakeDocker) Exec(context.Context, string, []string) (string, error)         { return "", nil }
func (f *fakeDocker) ExecTTY(context.Context, string, []string) error                { return nil }
func (f *fakeDocker) Copy(context.Context, string, string, string) error             { return nil }
func (f *fakeDocker) VolumeRuns(context.Context, string, string, string) bool        { return true }
func (f *fakeDocker) SeedFile(context.Context, string, string, string, string) error { return nil }
func (f *fakeDocker) SeedFromImage(context.Context, string, string, string) error    { return nil }

func (f *fakeDocker) ImageInfo(_ context.Context, image string) (provider.ImageInfo, error) {
	f.mu.Lock()
	err := f.inspectErr[image]
	f.mu.Unlock()

	if err != nil {
		return provider.ImageInfo{}, err
	}

	return provider.ImageInfo{Cmd: []string{"python3"}, OS: "linux", Arch: f.arch}, nil
}

func (f *fakeDocker) Pause(_ context.Context, ref string) error {
	f.mu.Lock()
	f.paused = append(f.paused, ref)
	f.mu.Unlock()

	return nil
}

func (f *fakeDocker) Unpause(context.Context, string) error { return nil }

func (f *fakeDocker) service(id string) spec.Service {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.created[id]
}

// fakeRuntime stands in for the daemon.
type fakeRuntime struct {
	mu    sync.Mutex
	calls []string
	held  map[string]bool
}

func (r *fakeRuntime) note(s string) {
	r.mu.Lock()
	r.calls = append(r.calls, s)
	r.mu.Unlock()
}

func (r *fakeRuntime) seen() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return strings.Join(r.calls, ",")
}

func (r *fakeRuntime) Refresh(context.Context)                   { r.note("refresh") }
func (r *fakeRuntime) Freeze(_ context.Context, id string) error { r.note("freeze " + id); return nil }
func (r *fakeRuntime) Thaw(_ context.Context, id string) error   { r.note("thaw " + id); return nil }
func (r *fakeRuntime) Hold(id string, held bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.held == nil {
		r.held = map[string]bool{}
	}

	r.held[id] = held
}

// harness is one API server over the fakes, with its state in a temp dir.
type harness struct {
	t    *testing.T
	srv  *Server
	http *httptest.Server
	p    *fakeDocker
	rt   *fakeRuntime
	eg   *fakeEgress
	dir  string

	mu      sync.Mutex
	now     time.Time
	pingErr error
}

type option func(*harness, *Options)

func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	t.Setenv("SBX_HISTORY", filepath.Join(t.TempDir(), "history.jsonl"))

	h := &harness{t: t, p: newFakeDocker(), rt: &fakeRuntime{}, eg: &fakeEgress{}, dir: t.TempDir(),
		now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}

	h.start(opts...)

	return h
}

func (h *harness) start(opts ...option) {
	o := Options{
		Provider:     h.p,
		Runtime:      h.rt,
		StateDir:     h.dir,
		Version:      "test",
		ReadyTimeout: 2 * time.Second,
		Now:          h.clock,
		Ping: func(context.Context, string) error {
			h.mu.Lock()
			defer h.mu.Unlock()

			return h.pingErr
		},
		Execd: func(context.Context, string) (execdSource, error) {
			return execdSource{Volume: "sbx-execd-test", File: "/dev/null"}, nil
		},
		LockSlots:    func() func() { return func() {} },
		Egress:       h.eg,
		EgressStatus: fakeEgressStatus,
	}

	for _, f := range opts {
		f(h, &o)
	}

	srv, err := New(o)
	if err != nil {
		h.t.Fatal(err)
	}

	h.srv = srv
	h.http = httptest.NewServer(srv.Handler())

	h.t.Cleanup(func() {
		h.http.Close()
		srv.Close()
	})
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

// do sends a request and decodes a JSON reply into out when out is non-nil.
func (h *harness) do(method, path string, body any, out any, hdr ...string) *http.Response {
	h.t.Helper()

	var rd io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rd = strings.NewReader(s)
		} else {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
	}

	req, err := http.NewRequest(method, h.http.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}

	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}

	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	resp.Body = io.NopCloser(bytes.NewReader(raw))

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s: decoding %q: %v", method, path, raw, err)
		}
	}

	return resp
}

// errOf reads an error body and checks it has the spec's shape.
func (h *harness) errOf(resp *http.Response) errorBody {
	h.t.Helper()

	var e errorBody
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Code == "" || e.Message == "" {
		h.t.Fatalf("status %d: body is not {code, message}: %+v %v", resp.StatusCode, e, err)
	}

	return e
}

func minimalCreate() map[string]any {
	return map[string]any{
		"image":          map[string]any{"uri": "python:3.11-slim"},
		"entrypoint":     []string{"tail", "-f", "/dev/null"},
		"resourceLimits": map[string]string{"cpu": "500m", "memory": "512Mi"},
		"timeout":        600,
	}
}

// create makes a sandbox and waits for it to leave Pending.
func (h *harness) create(body map[string]any) sandboxJSON {
	h.t.Helper()

	var sb sandboxJSON
	resp := h.do("POST", "/v1/sandboxes", body, &sb)

	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}

	return h.waitState(sb.ID, stateRunning, stateFailed)
}

func (h *harness) waitState(id string, states ...string) sandboxJSON {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		var sb sandboxJSON
		h.do("GET", "/v1/sandboxes/"+id, nil, &sb)

		for _, s := range states {
			if sb.Status.State == s {
				return sb
			}
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("%s stayed %s (%s: %s), wanted %v", id, sb.Status.State, sb.Status.Reason,
				sb.Status.Message, states)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

var errNotYet = errors.New("execd not listening yet")

// waitStateKeyed waits for Running on a server that requires the API key.
func (h *harness) waitStateKeyed(id string, key []string) {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		var sb sandboxJSON
		h.do("GET", "/v1/sandboxes/"+id, nil, &sb, key...)

		if sb.Status.State == stateRunning {
			return
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("%s stayed %s", id, sb.Status.State)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fakeDocker) Pull(_ context.Context, image string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pulls = append(f.pulls, image)

	return nil
}

func (f *fakeDocker) pulled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.pulls...)
}

func (f *fakeDocker) Commit(_ context.Context, ref, image string, changes ...string) error {
	f.mu.Lock()
	gate, err := f.commitGate, f.commitErr
	f.mu.Unlock()

	if gate != nil {
		<-gate
	}

	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.images == nil {
		f.images = map[string]bool{}
	}

	f.images[image] = true
	f.commits = append(f.commits, ref+" -> "+image)
	f.changes = append(f.changes, changes...)

	return nil
}

func (f *fakeDocker) Images(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []string

	for img := range f.images {
		if strings.HasPrefix(img, prefix) {
			out = append(out, img)
		}
	}

	return out, nil
}

func (f *fakeDocker) CopyVolume(context.Context, string, string) error { return nil }
func (f *fakeDocker) VolumeFor(sandbox, svc string) string {
	return "sbx-" + sandbox + "-" + svc + "-data"
}

func (f *fakeDocker) RemoveImage(_ context.Context, image string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.images, image)
	f.removedImg = append(f.removedImg, image)

	return nil
}

func (f *fakeDocker) VolumeExists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.volumes[name], f.volErr
}

func (f *fakeDocker) CreateVolume(_ context.Context, name string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.volumes == nil {
		f.volumes = map[string]bool{}
	}

	f.volumes[name] = true
	f.volCreated = append(f.volCreated, name)

	return nil
}

func (f *fakeDocker) RemoveVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.volumes, name)
	f.volRemoved = append(f.volRemoved, name)

	return nil
}

// lists returns a copy of one of the fake's recorded slices, under its lock.
func (f *fakeDocker) lists(which *[]string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), (*which)...)
}
