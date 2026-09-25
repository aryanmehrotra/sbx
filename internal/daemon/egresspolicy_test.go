package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// filterProvider answers EgressFilter with a fixed filter, and nothing else - the embedded nil
// Provider panics on anything these tests should not be reaching.
type filterProvider struct {
	provider.Provider
	f provider.EgressFilter
}

func (filterProvider) Name() string { return "fake" }

func (p *filterProvider) EgressFilter(_ context.Context, sandbox string) (provider.EgressFilter, error) {
	if sandbox != p.f.Sandbox {
		return provider.EgressFilter{}, provider.ErrNoSandbox
	}

	return p.f, nil
}

// containerFilter is a filter exactly as the container runs it: the real Filter behind the real
// Control handler, on a loopback listener.
func containerFilter(t *testing.T, start egress.Policy) (*egress.Filter, *httptest.Server) {
	t.Helper()

	f := egress.NewPolicy(start)
	srv := httptest.NewServer(&egress.Control{Filter: f, Token: "tok"})
	t.Cleanup(srv.Close)

	return f, srv
}

func declared() egress.Policy { return egress.FromAllowList([]string{"pypi.org"}) }

func newContainerSetup(t *testing.T) (*EgressControl, *egress.Filter, *filterProvider) {
	t.Helper()

	f, srv := containerFilter(t, declared())
	fp := &filterProvider{f: provider.EgressFilter{
		Sandbox: "osb-1", Gateway: "172.30.0.1", Services: []string{"sandbox"},
		Declared: declared(), Control: strings.TrimPrefix(srv.URL, "http://"), Token: "tok",
	}}

	return NewEgressControl(fp, t.TempDir()), f, fp
}

func TestPatchAndDeleteReachTheRunningContainerFilter(t *testing.T) {
	c, f, _ := newContainerSetup(t)
	ctx := context.Background()

	st, err := c.PatchPolicy(ctx, "osb-1", "sandbox", []egress.Rule{{Action: "deny", Target: "files.pypi.org"}})
	if err != nil {
		t.Fatal(err)
	}

	if f.Permits("files.pypi.org") || !f.Permits("pypi.org") {
		t.Fatalf("the running filter did not take the patch: %+v", f.Policy())
	}

	if st.Mode != "enforcing" || st.Policy.Egress[0].Target != "files.pypi.org" {
		t.Errorf("status does not show the merged policy: %+v", st)
	}

	if _, err := c.DeleteRules(ctx, "osb-1", "", []string{"FILES.pypi.org", "absent.com"}); err != nil {
		t.Fatal(err)
	}

	if !f.Permits("files.pypi.org") {
		t.Fatal("DELETE of the deny rule did not reach the running filter")
	}

	if _, err := c.SetDefault(ctx, "osb-1", "", "allow"); err != nil {
		t.Fatal(err)
	}

	if !f.Permits("anything.example") {
		t.Fatal("changing the default did not reach the running filter")
	}

	got, err := c.GetPolicy(ctx, "osb-1", "sandbox")
	if err != nil || got.Policy.DefaultAction != "allow" || len(got.Policy.Egress) != len(declared().Egress) {
		t.Fatalf("GET after the changes: %+v %v", got, err)
	}
}

func TestResetReturnsToTheDeclarationAndForgetsTheLiveCopy(t *testing.T) {
	c, f, _ := newContainerSetup(t)
	ctx := context.Background()

	if _, err := c.SetPolicy(ctx, "osb-1", "", egress.Policy{DefaultAction: "allow"}); err != nil {
		t.Fatal(err)
	}

	if _, ok := c.load("osb-1"); !ok {
		t.Fatal("a live change was not saved on the host")
	}

	if _, err := c.ResetPolicy(ctx, "osb-1", ""); err != nil {
		t.Fatal(err)
	}

	if f.Policy().Hash() != declared().Hash() {
		t.Fatalf("reset left %+v in force", f.Policy())
	}

	if _, ok := c.load("osb-1"); ok {
		t.Fatal("reset kept the saved live policy, which a restart would bring back")
	}
}

// A container filter that was replaced starts from its declaration; Sync puts the live policy
// back. One made against an older declaration is an exception to a policy that no longer
// exists, and is dropped instead.
func TestSyncRestoresTheLivePolicyToAReplacedFilter(t *testing.T) {
	c, _, fp := newContainerSetup(t)
	ctx := context.Background()

	if _, err := c.PatchPolicy(ctx, "osb-1", "", []egress.Rule{{Action: "deny", Target: "pypi.org"}}); err != nil {
		t.Fatal(err)
	}

	fresh, srv := containerFilter(t, declared())
	fp.f.Control = strings.TrimPrefix(srv.URL, "http://")

	if err := c.Sync(ctx, "osb-1"); err != nil {
		t.Fatal(err)
	}

	if fresh.Permits("pypi.org") {
		t.Fatal("Sync did not restore the live deny to a replaced filter")
	}

	fp.f.Declared = egress.FromAllowList([]string{"other.org"})

	if err := c.Sync(ctx, "osb-1"); err != nil {
		t.Fatal(err)
	}

	if _, ok := c.load("osb-1"); ok {
		t.Fatal("a live policy made against a different declaration survived a recreate")
	}
}

func TestWritesThatNameOneOfSeveralSharingServicesAreRefused(t *testing.T) {
	c, _, fp := newContainerSetup(t)
	fp.f.Services = []string{"api", "worker"}
	ctx := context.Background()

	_, err := c.PatchPolicy(ctx, "osb-1", "api", []egress.Rule{{Action: "deny", Target: "x.com"}})
	if !errors.Is(err, ErrSharedFilter) || EgressHTTPStatus(err) != http.StatusConflict {
		t.Fatalf("a write for one of two services sharing a filter: %v", err)
	}

	if _, err := c.GetPolicy(ctx, "osb-1", "api"); err != nil {
		t.Fatalf("reading through one service of a shared filter was refused: %v", err)
	}

	if _, err := c.GetPolicy(ctx, "osb-1", "db"); !errors.Is(err, provider.ErrNotFiltered) {
		t.Fatalf("a service that is not filtered: %v", err)
	}

	if _, err := c.GetPolicy(ctx, "nope", ""); EgressHTTPStatus(err) != http.StatusNotFound {
		t.Fatalf("an unknown sandbox: %v", err)
	}

	_, err = c.SetPolicy(ctx, "osb-1", "", egress.Policy{DefaultAction: "maybe"})
	if EgressHTTPStatus(err) != http.StatusBadRequest {
		t.Fatalf("an invalid policy: %v", err)
	}
}

// The sidecar-shaped handler, against upstream's own documented requests.
func TestHandlerSpeaksTheSidecarPolicyAPI(t *testing.T) {
	c, f, _ := newContainerSetup(t)
	h := c.Handler("osb-1")

	do := func(method, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/policy", strings.NewReader(body)))

		return rec
	}

	if rec := do(http.MethodPatch, `[{"action":"allow","target":"example.com"},{"action":"deny","target":"example.com"}]`); rec.Code != 200 {
		t.Fatalf("PATCH: %d %s", rec.Code, rec.Body)
	}

	if !f.Permits("example.com") {
		t.Fatal("within one patch the first rule for a target must win (upstream's own example)")
	}

	if rec := do(http.MethodPatch, `[]`); rec.Code != 400 {
		t.Fatalf("an empty PATCH got %d, want 400", rec.Code)
	}

	if rec := do(http.MethodDelete, `["example.com"]`); rec.Code != 200 || f.Permits("example.com") {
		t.Fatalf("DELETE: %d, still permits=%v", rec.Code, f.Permits("example.com"))
	}

	if rec := do(http.MethodPut, ``); rec.Code != 200 || f.Policy().Mode() != "deny_all" {
		t.Fatalf("an empty PUT must reset to deny-all: %d %+v", rec.Code, f.Policy())
	}

	rec := do(http.MethodGet, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"mode":"deny_all"`) {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
}

// A filter hosted by the daemon: the daemon's own API swaps it at once, and a change written by
// another process (the CLI) is picked up from the saved copy.
func TestHostedFiltersChangeInPlace(t *testing.T) {
	dir := t.TempDir()
	fp := &filterProvider{f: provider.EgressFilter{
		Sandbox: "box", Gateway: "172.31.0.1", Services: []string{"agent"}, Declared: declared(),
	}}

	filter := egress.NewPolicy(declared())
	d := &daemon{provider: fp, egressDir: dir, egress: map[string]*egressProxy{
		"172.31.0.1": {sandbox: "box", filter: filter, declared: declared()},
	}}

	if _, err := d.Egress().PatchPolicy(context.Background(), "box", "", []egress.Rule{{Action: "deny", Target: "pypi.org"}}); err != nil {
		t.Fatal(err)
	}

	if filter.Permits("pypi.org") {
		t.Fatal("the daemon's own API did not swap the policy of the filter it hosts")
	}

	// The CLI's copy has no hook into this process; it writes the saved policy.
	cli := NewEgressControl(fp, dir)

	st, err := cli.SetPolicy(context.Background(), "box", "agent", egress.Policy{DefaultAction: "allow"})
	if err != nil {
		t.Fatal(err)
	}

	if st.Reason == "" {
		t.Error("a policy the CLI only saved is reported as if it were in force")
	}

	d.refreshHosted("box")

	if !filter.Permits("anything.example") {
		t.Fatal("the daemon did not pick up a policy the CLI saved")
	}

	if _, err := os.Stat(filepath.Join(dir, "box.json")); err != nil {
		t.Fatal(err)
	}
}
