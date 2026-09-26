package osb

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// fakeVM is a provider whose sandboxes run execd themselves, the way a microVM's PID 1 does:
// RunsAgent, and neither an Injector nor able to mount host directories. Everything else - the
// pauser, snapshots, named volumes - is the fake's.
type fakeVM struct{ *fakeDocker }

func (fakeVM) Name() string { return "firecracker" }
func (fakeVM) RunsAgent()   {}

// Shadowed with other signatures, so fakeVM is not an Injector and cannot mount host paths.
func (fakeVM) VolumeRuns()      {}
func (fakeVM) HostVolumes(bool) {}

var (
	_ provider.RunsAgent   = fakeVM{}
	_ provider.Snapshotter = fakeVM{}
)

func newVMHarness(t *testing.T, opts ...option) *harness {
	t.Helper()

	return newHarness(t, append([]option{func(h *harness, o *Options) { o.Provider = fakeVM{h.p} }}, opts...)...)
}

// A provider that runs the agent gets the workload's own command, the API's token in the env and
// nothing of the container path: no /opt/sbx volume, no execd wrapper, no health command that
// would run the agent from that volume.
func TestRunsAgentCreateAsksForTheWorkloadNotTheWrapper(t *testing.T) {
	h := newVMHarness(t)

	b := minimalCreate()
	b["env"] = map[string]string{"A": "1"}
	sb := h.create(b)

	if sb.Status.State != stateRunning {
		t.Fatalf("create = %+v", sb.Status)
	}

	svc := h.p.service(sb.ID)
	rec, _ := h.srv.snapshot(sb.ID)

	switch {
	case !slices.Equal(svc.Entrypoint, []string{"tail", "-f", "/dev/null"}):
		t.Errorf("entrypoint %q: a RunsAgent provider runs the workload under its own execd, unwrapped", svc.Entrypoint)
	case len(svc.ReadOnlyVolumes) != 0:
		t.Errorf("read-only volumes %v: there is no execd volume to mount", svc.ReadOnlyVolumes)
	case svc.Health != "" || svc.HealthInterval != "" || svc.HealthStartInterval != "":
		t.Errorf("health %q: it would run the agent from /opt/sbx, which is not mounted", svc.Health)
	case svc.Env[tokenEnv] == "" || svc.Env[tokenEnv] != rec.Token:
		t.Errorf("env token %q, record token %q: the provider must boot execd with the API's token", svc.Env[tokenEnv], rec.Token)
	case svc.Env["A"] != "1":
		t.Errorf("env = %v", svc.Env)
	case !slices.Equal(svc.Ports, []int{execdPort}):
		t.Errorf("ports %v", svc.Ports)
	}

	// No entrypoint given: the image's command, as recorded, and still unwrapped.
	b = minimalCreate()
	delete(b, "entrypoint")
	sb = h.create(b)

	if svc := h.p.service(sb.ID); !slices.Equal(svc.Entrypoint, []string{"python3"}) || !slices.Equal(sb.Entrypoint, []string{"python3"}) {
		t.Errorf("no entrypoint: svc %q, record %q, want the image's CMD", svc.Entrypoint, sb.Entrypoint)
	}
}

// The token execd is booted with is the one the endpoint hands out: one token per sandbox.
func TestRunsAgentEndpointCarriesTheBootToken(t *testing.T) {
	h := newVMHarness(t)
	sb := h.create(minimalCreate())

	var ep endpointJSON
	h.do("GET", "/v1/sandboxes/"+sb.ID+"/endpoints/44772", nil, &ep)

	if got := h.p.service(sb.ID).Env[tokenEnv]; ep.Headers[tokenHeader] != got || got == "" {
		t.Fatalf("endpoint token %q, boot token %q", ep.Headers[tokenHeader], got)
	}

	var other endpointJSON
	h.do("GET", "/v1/sandboxes/"+sb.ID+"/endpoints/8080", nil, &other)

	if !strings.HasSuffix(other.Endpoint, "/proxy/8080") || other.Headers[tokenHeader] != ep.Headers[tokenHeader] {
		t.Fatalf("undeclared port = %+v, want execd's /proxy with the token", other)
	}
}

// Every lifecycle route answers on a RunsAgent provider as it does on docker.
func TestRunsAgentLifecycleRoutes(t *testing.T) {
	h := newVMHarness(t)
	id := h.create(minimalCreate()).ID
	base := "/v1/sandboxes/" + id

	var list listJSON
	if resp := h.do("GET", "/v1/sandboxes", nil, &list); resp.StatusCode != 200 || len(list.Items) != 1 {
		t.Fatalf("list = %d %+v", resp.StatusCode, list)
	}

	if resp := h.do("PATCH", base+"/metadata", map[string]any{"team": "a"}, nil); resp.StatusCode != 200 {
		t.Errorf("metadata = %d", resp.StatusCode)
	}

	next := h.clock().Add(time.Hour).Format(time.RFC3339)
	if resp := h.do("POST", base+"/renew-expiration", map[string]string{"expiresAt": next}, nil); resp.StatusCode != 200 {
		t.Errorf("renew = %d %+v", resp.StatusCode, h.errOf(resp))
	}

	for _, kind := range []string{"logs", "events"} {
		var d diagnosticJSON
		if resp := h.do("GET", base+"/diagnostics/"+kind+"?scope=all", nil, &d); resp.StatusCode != 200 || d.Content == "" {
			t.Errorf("diagnostics/%s = %d %+v", kind, resp.StatusCode, d)
		}
	}

	if resp := h.do("POST", base+"/pause", nil, nil); resp.StatusCode != 202 {
		t.Fatalf("pause = %d", resp.StatusCode)
	}

	h.waitState(id, statePaused)

	if resp := h.do("POST", base+"/resume", nil, nil); resp.StatusCode != 202 {
		t.Fatalf("resume = %d", resp.StatusCode)
	}

	h.waitState(id, stateRunning)

	if got := h.rt.seen(); !strings.Contains(got, "freeze "+id) || !strings.Contains(got, "thaw "+id) {
		t.Errorf("the daemon saw %q; pause is the provider's VM pause through the daemon", got)
	}

	if resp := h.do("DELETE", base, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("delete = %d", resp.StatusCode)
	}

	if resp := h.do("GET", base, nil, nil); resp.StatusCode != 404 {
		t.Errorf("get after delete = %d", resp.StatusCode)
	}
}

// A host volume is refused by name on a provider that cannot mount one - before, and whatever,
// the operator's allow-list says; a pvc goes through as a named volume the provider attaches.
func TestRunsAgentVolumes(t *testing.T) {
	root, allow := hostRoot(t)
	h := newVMHarness(t, allow)

	resp := h.do("POST", "/v1/sandboxes", withVolumes(map[string]any{"name": "work",
		"host": map[string]any{"path": root + "/w"}, "mountPath": "/work"}), nil)

	if e := h.errOf(resp); resp.StatusCode != http.StatusNotImplemented || !strings.Contains(e.Message, "host.path") ||
		!strings.Contains(e.Message, "virtio-fs") {
		t.Fatalf("host volume on a VM = %d %+v", resp.StatusCode, e)
	}

	sb := h.create(withVolumes(map[string]any{"name": "d", "pvc": map[string]any{"claimName": "data"},
		"mountPath": "/data", "subPath": "x"}))

	want := []spec.VolumeMount{{Volume: "sbx-osb-pvc-data", Target: "/data", SubPath: "x"}}
	if got := h.p.service(sb.ID).VolumeMounts; !slices.Equal(got, want) {
		t.Fatalf("mounts = %+v, want %+v", got, want)
	}
}

// The warm pool is the next phase on a VM: asked for, it is a startup error naming it.
func TestRunsAgentRefusesAPoolAtStartup(t *testing.T) {
	h := newVMHarness(t)

	_, err := New(Options{Provider: fakeVM{h.p}, Runtime: h.rt, StateDir: t.TempDir(), Version: "test",
		Pools: []PoolSpec{{Image: "python:3.11-slim", Size: 1}}})
	if err == nil || !strings.Contains(err.Error(), "--osb-pool on the firecracker provider") {
		t.Fatalf("New with a pool = %v", err)
	}
}

// A VM snapshot is its disk: nothing to scrub, and a microVM snapshot takes no image changes.
// A fork from it is a new sandbox with its own token, created from the snapshot's name.
func TestRunsAgentSnapshotAndFork(t *testing.T) {
	h := newVMHarness(t)
	src, sj := h.snapshotOf(nil)

	if ch := h.p.snapshotOf(&h.p.changes); len(ch) != 0 {
		t.Fatalf("commit changes %v on a RunsAgent provider", ch)
	}

	fork := h.create(map[string]any{"snapshotId": sj.ID, "timeout": 600})
	if fork.Status.State != stateRunning {
		t.Fatalf("fork = %+v", fork.Status)
	}

	svc := h.p.service(fork.ID)
	srcRec, _ := h.srv.snapshot(src.ID)
	forkRec, _ := h.srv.snapshot(fork.ID)

	if svc.Image != "sbx-osb-snap:"+sj.ID || !slices.Equal(svc.Entrypoint, []string{"tail", "-f", "/dev/null"}) {
		t.Errorf("fork asked for %q running %q", svc.Image, svc.Entrypoint)
	}

	if forkRec.Token == srcRec.Token || svc.Env[tokenEnv] != forkRec.Token {
		t.Error("the fork must boot with its own token")
	}

	// And a template from that snapshot creates the same way.
	tj := h.template(map[string]any{"image": sj.ID})
	h.waitTpl(tj.TemplateID, tplSucceeded)

	if sb := h.create(map[string]any{"templateId": tj.TemplateID, "timeout": 600}); sb.Status.State != stateRunning {
		t.Fatalf("from template = %+v", sb.Status)
	}
}
