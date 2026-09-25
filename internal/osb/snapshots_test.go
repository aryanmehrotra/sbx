package osb

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

func (h *harness) waitSnap(id string, states ...string) snapshotJSON {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		var sj snapshotJSON
		h.do("GET", "/v1/snapshots/"+id, nil, &sj)

		if slices.Contains(states, sj.Status.State) {
			return sj
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("snapshot %s stayed %s (%s), wanted %v", id, sj.Status.State, sj.Status.Message, states)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// snapshotOf takes a snapshot of a new Running sandbox and waits for it to be Ready.
func (h *harness) snapshotOf(body any) (sandboxJSON, snapshotJSON) {
	h.t.Helper()

	sb := h.create(minimalCreate())

	var sj snapshotJSON
	if resp := h.do("POST", "/v1/sandboxes/"+sb.ID+"/snapshots", body, &sj); resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("snapshot = %d %+v", resp.StatusCode, h.errOf(resp))
	}

	return sb, h.waitSnap(sj.ID, snapReady)
}

func TestSnapshotIsACommitThatGoesCreatingThenReady(t *testing.T) {
	h := newHarness(t)
	sb := h.create(minimalCreate())

	gate := make(chan struct{})
	h.p.mu.Lock()
	h.p.commitGate = gate
	h.p.mu.Unlock()

	var sj snapshotJSON
	resp := h.do("POST", "/v1/sandboxes/"+sb.ID+"/snapshots", map[string]string{"name": "before-import"}, &sj)

	if resp.StatusCode != http.StatusAccepted || sj.Status.State != snapCreating || sj.Name != "before-import" ||
		sj.SandboxID != sb.ID || !snapIDPattern.MatchString(sj.ID) {
		t.Fatalf("create snapshot = %d %+v", resp.StatusCode, sj)
	}

	if loc := resp.Header.Get("Location"); loc != "/v1/snapshots/"+sj.ID {
		t.Errorf("Location = %q", loc)
	}

	// Still Creating while the commit runs, and a DELETE now is refused.
	if got := h.waitSnap(sj.ID, snapCreating); got.Status.Reason != "snapshot_accepted" {
		t.Errorf("creating reason %q", got.Status.Reason)
	}

	if resp := h.do("DELETE", "/v1/snapshots/"+sj.ID, nil, nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("DELETE while Creating = %d, want 409", resp.StatusCode)
	}

	close(gate)

	ready := h.waitSnap(sj.ID, snapReady)
	if ready.Status.Reason != "snapshot_ready" {
		t.Errorf("ready reason %q", ready.Status.Reason)
	}

	commits := h.p.snapshotOf(&h.p.commits)
	if len(commits) != 1 || commits[0] != "sbx-"+sb.ID+"-sandbox -> sbx-osb-snap:"+sj.ID {
		t.Fatalf("commits = %v: the snapshot must be the sandbox's own container committed to sbx-osb-snap:<id>", commits)
	}
}

func TestSnapshotCommitFailureIsFailedWithTheReason(t *testing.T) {
	h := newHarness(t)
	sb := h.create(minimalCreate())

	h.p.mu.Lock()
	h.p.commitErr = errors.New("no space left on device")
	h.p.mu.Unlock()

	var sj snapshotJSON
	h.do("POST", "/v1/sandboxes/"+sb.ID+"/snapshots", nil, &sj)

	got := h.waitSnap(sj.ID, snapFailed)
	if got.Status.Reason != "snapshot_capture_failed" || !strings.Contains(got.Status.Message, "no space left") {
		t.Fatalf("failed status = %+v", got.Status)
	}

	// A Failed snapshot has no image, so deleting it removes only the record.
	if resp := h.do("DELETE", "/v1/snapshots/"+sj.ID, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE Failed = %d", resp.StatusCode)
	}

	if rm := h.p.snapshotOf(&h.p.removedImg); len(rm) != 0 {
		t.Errorf("removed images %v for a snapshot that never had one", rm)
	}
}

func TestSnapshotRefusals(t *testing.T) {
	h := newHarness(t)
	sb := h.create(minimalCreate())

	cases := []struct {
		name, path string
		body       any
		status     int
		code       string
	}{
		{"unknown sandbox", "/v1/sandboxes/osb-000000000000/snapshots", nil, 404, "SANDBOX::NOT_FOUND"},
		{"unknown field", "/v1/sandboxes/" + sb.ID + "/snapshots", `{"label":"x"}`, 400, "SANDBOX::INVALID_PARAMETER"},
		{"empty name", "/v1/sandboxes/" + sb.ID + "/snapshots", `{"name":""}`, 400, "SANDBOX::INVALID_PARAMETER"},
	}

	for _, c := range cases {
		resp := h.do("POST", c.path, c.body, nil)
		if resp.StatusCode != c.status || h.errOf(resp).Code != c.code {
			t.Errorf("%s: %d, want %d %s", c.name, resp.StatusCode, c.status, c.code)
		}
	}

	// Paused is not Running, and the spec requires Running.
	h.do("POST", "/v1/sandboxes/"+sb.ID+"/pause", nil, nil)

	resp := h.do("POST", "/v1/sandboxes/"+sb.ID+"/snapshots", nil, nil)
	if e := h.errOf(resp); resp.StatusCode != http.StatusConflict || e.Code != "SNAPSHOT::INVALID_SOURCE_STATE" ||
		!strings.Contains(e.Message, "resume") {
		t.Fatalf("snapshot of a paused sandbox = %d %+v", resp.StatusCode, e)
	}

	for _, p := range []string{"/v1/snapshots/snap-000000000000", "/v1/snapshots/../../etc"} {
		if resp := h.do("GET", p, nil, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestListSnapshotsFiltersAndPages(t *testing.T) {
	h := newHarness(t)

	a, sa := h.snapshotOf(map[string]string{"name": "one"})
	h.advance(time.Second)
	_, sb2 := h.snapshotOf(map[string]string{"name": "two"})
	h.advance(time.Second)

	var sa2 snapshotJSON
	h.do("POST", "/v1/sandboxes/"+a.ID+"/snapshots", nil, &sa2)
	h.waitSnap(sa2.ID, snapReady)

	ids := func(q string) []string {
		var l listSnapshotsJSON
		if resp := h.do("GET", "/v1/snapshots"+q, nil, &l); resp.StatusCode != 200 {
			t.Fatalf("list %s = %d", q, resp.StatusCode)
		}

		var out []string
		for _, s := range l.Items {
			out = append(out, s.ID)
		}

		return out
	}

	if got := ids(""); !slices.Equal(got, []string{sa.ID, sb2.ID, sa2.ID}) {
		t.Errorf("all = %v, want oldest first", got)
	}

	if got := ids("?sandboxId=" + a.ID); !slices.Equal(got, []string{sa.ID, sa2.ID}) {
		t.Errorf("by sandbox = %v", got)
	}

	if got := ids("?name=two"); !slices.Equal(got, []string{sb2.ID}) {
		t.Errorf("by name = %v", got)
	}

	if got := ids("?state=Failed&state=Creating"); len(got) != 0 {
		t.Errorf("by state = %v", got)
	}

	var l listSnapshotsJSON
	h.do("GET", "/v1/snapshots?page=2&pageSize=2", nil, &l)

	if len(l.Items) != 1 || l.Pagination.TotalItems != 3 || l.Pagination.TotalPages != 2 || l.Pagination.HasNextPage {
		t.Errorf("page 2 = %+v", l)
	}

	if resp := h.do("GET", "/v1/snapshots?pageSize=0", nil, nil); resp.StatusCode != 400 {
		t.Errorf("pageSize=0 = %d", resp.StatusCode)
	}
}

// Restoring is a fork: a new sandbox, a new token, the snapshot's image, and no pull - the
// image exists only on this engine.
func TestCreateFromSnapshotIsAFork(t *testing.T) {
	h := newHarness(t)
	src, sj := h.snapshotOf(nil)

	body := map[string]any{"snapshotId": sj.ID, "timeout": 600,
		"resourceLimits": map[string]string{"cpu": "500m", "memory": "512Mi"}}

	fork := h.create(body)
	if fork.Status.State != stateRunning || fork.ID == src.ID {
		t.Fatalf("fork = %+v", fork)
	}

	if fork.Image == nil || fork.Image.URI != "sbx-osb-snap:"+sj.ID {
		t.Errorf("fork image = %+v", fork.Image)
	}

	if !slices.Equal(fork.Entrypoint, []string{"tail", "-f", "/dev/null"}) {
		t.Errorf("entrypoint = %v, want the spec's restore default", fork.Entrypoint)
	}

	if slices.Contains(h.p.pulled(), "sbx-osb-snap:"+sj.ID) {
		t.Error("a snapshot image was pulled; it exists only on this engine")
	}

	svc := h.p.service(fork.ID)
	if svc.Image != "sbx-osb-snap:"+sj.ID {
		t.Errorf("container image %q", svc.Image)
	}

	srcRec, _ := h.srv.snapshot(src.ID)
	forkRec, _ := h.srv.snapshot(fork.ID)

	if forkRec.Token == srcRec.Token || svc.Env[tokenEnv] != forkRec.Token {
		t.Error("the fork must get its own execd token, passed in its env")
	}

	// An explicit entrypoint wins over the default.
	body["entrypoint"] = []string{"python", "/workspace/app.py"}
	if withEP := h.create(body); !slices.Equal(withEP.Entrypoint, []string{"python", "/workspace/app.py"}) {
		t.Errorf("entrypoint = %v", withEP.Entrypoint)
	}

	// While forks exist the snapshot cannot be deleted; once they are gone it can.
	resp := h.do("DELETE", "/v1/snapshots/"+sj.ID, nil, nil)
	if e := h.errOf(resp); resp.StatusCode != http.StatusConflict || !strings.Contains(e.Message, fork.ID) {
		t.Fatalf("DELETE in use = %d %+v", resp.StatusCode, e)
	}

	var l listJSON
	h.do("GET", "/v1/sandboxes", nil, &l)

	for _, sb := range l.Items {
		h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, nil)
	}

	if resp := h.do("DELETE", "/v1/snapshots/"+sj.ID, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d", resp.StatusCode)
	}

	if rm := h.p.snapshotOf(&h.p.removedImg); !slices.Equal(rm, []string{"sbx-osb-snap:" + sj.ID}) {
		t.Errorf("removed images %v", rm)
	}

	if resp := h.do("GET", "/v1/snapshots/"+sj.ID, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d", resp.StatusCode)
	}
}

func TestCreateFromSnapshotRefusals(t *testing.T) {
	h := newHarness(t)
	sb := h.create(minimalCreate())

	resp := h.do("POST", "/v1/sandboxes", map[string]any{"snapshotId": "snap-000000000000"}, nil)
	if e := h.errOf(resp); resp.StatusCode != http.StatusNotFound || e.Code != "SNAPSHOT::NOT_FOUND" {
		t.Errorf("missing snapshot = %d %+v", resp.StatusCode, e)
	}

	gate := make(chan struct{})
	defer close(gate)

	h.p.mu.Lock()
	h.p.commitGate = gate
	h.p.mu.Unlock()

	var sj snapshotJSON
	h.do("POST", "/v1/sandboxes/"+sb.ID+"/snapshots", nil, &sj)

	resp = h.do("POST", "/v1/sandboxes", map[string]any{"snapshotId": sj.ID}, nil)
	if e := h.errOf(resp); resp.StatusCode != http.StatusConflict || e.Code != "SNAPSHOT::NOT_READY" {
		t.Errorf("creating snapshot = %d %+v", resp.StatusCode, e)
	}
}

// A restart during a commit leaves the record Creating. What docker has decides it.
func TestSnapshotsInterruptedByARestartAreSettledFromDocker(t *testing.T) {
	h := newHarness(t)

	// Two records a daemon left Creating when it died: one whose commit had finished (its
	// image exists) and one whose had not.
	now := h.clock()

	for _, id := range []string{"snap-00000000000a", "snap-00000000000b"} {
		rec := snapshotRecord{ID: id, SandboxID: "osb-000000000001", Image: snapRepo + ":" + id,
			State: snapCreating, CreatedAt: now, LastTransitionAt: now}
		if err := writeAtomic(filepath.Join(h.dir, "snapshots"), id, rec); err != nil {
			t.Fatal(err)
		}
	}

	h.p.mu.Lock()
	h.p.images = map[string]bool{snapRepo + ":snap-00000000000a": true}
	h.p.mu.Unlock()

	h.start()
	h.srv.recover(context.Background())

	if got := h.waitSnap("snap-00000000000a", snapReady, snapFailed); got.Status.State != snapReady {
		t.Errorf("snapshot whose image exists = %+v", got.Status)
	}

	if got := h.waitSnap("snap-00000000000b", snapReady, snapFailed); got.Status.State != snapFailed ||
		got.Status.Reason != "snapshot_recovery_missing_image" {
		t.Errorf("snapshot with no image = %+v", got.Status)
	}
}

// coreOnly is a provider with the core and injection only: no snapshots, no named volumes.
type coreOnly struct {
	provider.Provider
	provider.Injector
}

func TestSnapshotsAndClaimsNeedTheCapability(t *testing.T) {
	h := newHarness(t, func(h *harness, o *Options) { o.Provider = coreOnly{h.p, h.p} })
	sb := h.create(minimalCreate())

	resp := h.do("POST", "/v1/sandboxes/"+sb.ID+"/snapshots", nil, nil)
	if e := h.errOf(resp); resp.StatusCode != http.StatusNotImplemented || !strings.Contains(e.Message, "cannot snapshot") {
		t.Errorf("snapshot on a provider without Snapshotter = %d %+v", resp.StatusCode, e)
	}

	body := minimalCreate()
	body["volumes"] = []any{map[string]any{"name": "d", "pvc": map[string]any{"claimName": "data"}, "mountPath": "/data"}}

	resp = h.do("POST", "/v1/sandboxes", body, nil)
	if e := h.errOf(resp); resp.StatusCode != http.StatusNotImplemented || !strings.Contains(e.Message, "named volumes") {
		t.Errorf("pvc on a provider without NamedVolumes = %d %+v", resp.StatusCode, e)
	}
}

// The source may still be running, so its execd token is live; a snapshot image that carried it
// would be a credential for the source handed to anyone who can read the image.
func TestSnapshotCommitScrubsPerSandboxEnv(t *testing.T) {
	h := newHarness(t)
	h.snapshotOf(nil)

	changes := h.p.snapshotOf(&h.p.changes)
	for _, k := range []string{"EXECD_ACCESS_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if !slices.Contains(changes, "ENV "+k+"=") {
			t.Errorf("commit changes %v do not clear %s", changes, k)
		}
	}
}
