package osb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/egress"
)

// fakeEgress stands in for the daemon's EgressControl: one policy per sandbox, and a record of
// what it was asked, so a test checks what reached it rather than what the API printed.
type fakeEgress struct {
	mu      sync.Mutex
	pol     map[string]egress.Policy
	calls   []string
	missing bool // every sandbox is unfiltered, as one created without networkPolicy is
}

var errUnfiltered = errors.New("no egress filter")

func fakeEgressStatus(err error) int {
	var bad *egress.Error

	switch {
	case errors.As(err, &bad):
		return http.StatusBadRequest
	case errors.Is(err, errUnfiltered):
		return http.StatusConflict
	}

	return http.StatusInternalServerError
}

func (f *fakeEgress) note(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeEgress) seen() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return strings.Join(f.calls, ",")
}

func (f *fakeEgress) get(id string) (egress.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.missing {
		return egress.Policy{}, errUnfiltered
	}

	if f.pol == nil {
		f.pol = map[string]egress.Policy{}
	}

	p, ok := f.pol[id]
	if !ok {
		p = egress.Policy{DefaultAction: "allow", Egress: []egress.Rule{}}
	}

	return p, nil
}

func (f *fakeEgress) set(id string, p egress.Policy) egress.Status {
	f.mu.Lock()
	f.pol[id] = p
	f.mu.Unlock()

	return egress.StatusOf(p, "")
}

func (f *fakeEgress) GetPolicy(_ context.Context, id, _ string) (egress.Status, error) {
	f.note("get " + id)

	p, err := f.get(id)

	return egress.StatusOf(p, ""), err
}

func (f *fakeEgress) SetPolicy(_ context.Context, id, _ string, p egress.Policy) (egress.Status, error) {
	f.note("set " + id + " " + p.DefaultAction)

	if _, err := f.get(id); err != nil {
		return egress.Status{}, err
	}

	return f.set(id, p), nil
}

func (f *fakeEgress) PatchPolicy(_ context.Context, id, _ string, rules []egress.Rule) (egress.Status, error) {
	f.note("patch " + id)

	cur, err := f.get(id)
	if err != nil {
		return egress.Status{}, err
	}

	return f.set(id, cur.Merge(rules)), nil
}

func (f *fakeEgress) DeleteRules(_ context.Context, id, _ string, targets []string) (egress.Status, error) {
	f.note("delete " + id + " " + strings.Join(targets, "+"))

	cur, err := f.get(id)
	if err != nil {
		return egress.Status{}, err
	}

	p, _ := cur.Remove(targets)

	return f.set(id, p), nil
}

func (f *fakeEgress) Handler(id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.note("sidecar " + r.Method + " " + id)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","mode":"allow_all"}`))
	})
}

func (f *fakeEgress) Forget(id string) error {
	f.note("forget " + id)
	return nil
}

func TestNetworkPolicyRoutesReachTheLivePolicy(t *testing.T) {
	h := newHarness(t)
	id := h.create(minimalCreate()).ID
	path := "/v1/sandboxes/" + id + "/networkpolicy"

	var st egress.Status

	if resp := h.do("PUT", path, `{"defaultAction":"deny","egress":[{"action":"allow","target":"pypi.org"}]}`, &st); resp.StatusCode != 200 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}

	if st.Policy == nil || st.Policy.DefaultAction != "deny" || len(st.Policy.Egress) != 1 {
		t.Fatalf("PUT answered %+v", st)
	}

	if resp := h.do("PATCH", path, `[{"action":"allow","target":"*.python.org"}]`, &st); resp.StatusCode != 200 || len(st.Policy.Egress) != 2 {
		t.Fatalf("PATCH = %d %+v", resp.StatusCode, st.Policy)
	}

	if resp := h.do("DELETE", path, `["pypi.org"]`, &st); resp.StatusCode != 200 || len(st.Policy.Egress) != 1 {
		t.Fatalf("DELETE = %d %+v", resp.StatusCode, st.Policy)
	}

	if resp := h.do("GET", path, nil, &st); resp.StatusCode != 200 || st.Policy.Egress[0].Target != "*.python.org" {
		t.Fatalf("GET = %d %+v", resp.StatusCode, st.Policy)
	}

	want := "set " + id + " deny,patch " + id + ",delete " + id + " pypi.org,get " + id
	if got := h.eg.seen(); got != want {
		t.Fatalf("the egress API saw %q, want %q", got, want)
	}

	// Refusals keep egress's own message, in the lifecycle API's error shape.
	for body, method := range map[string]string{
		`{"defaultAction":"maybe"}`: "PUT",
		`[]`:                        "PATCH",
		`not json`:                  "DELETE",
	} {
		resp := h.do(method, path, body, nil)
		if resp.StatusCode != 400 {
			t.Errorf("%s %s = %d, want 400", method, body, resp.StatusCode)
			continue
		}

		h.errOf(resp)
	}

	if resp := h.do("GET", "/v1/sandboxes/osb-000000000000/networkpolicy", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("unknown sandbox = %d", resp.StatusCode)
	}
}

// A sandbox created without a networkPolicy has no filter to change: say so, with the fix.
func TestNetworkPolicyOnAnUnfilteredSandboxIsAConflict(t *testing.T) {
	h := newHarness(t)
	id := h.create(minimalCreate()).ID
	h.eg.missing = true

	resp := h.do("GET", "/v1/sandboxes/"+id+"/networkpolicy", nil, nil)
	if resp.StatusCode != 409 || !strings.Contains(h.errOf(resp).Message, "networkPolicy") {
		t.Fatalf("GET on an unfiltered sandbox = %d", resp.StatusCode)
	}
}

func TestCreateCarriesNetworkPolicyToTheSpec(t *testing.T) {
	h := newHarness(t)

	with := func(np any) map[string]any {
		b := minimalCreate()
		if np != nil {
			b["networkPolicy"] = np
		}

		return b
	}

	sb := h.create(with(map[string]any{"defaultAction": "deny", "egress": []any{
		map[string]string{"action": "allow", "target": "pypi.org"},
		map[string]string{"action": "allow", "target": "*.python.org"},
	}}))

	p := h.p.service(sb.ID).EgressPolicy
	if p == nil || p.DefaultAction != "deny" || len(p.Egress) != 2 || p.Egress[1].Target != "*.python.org" {
		t.Fatalf("egress_policy = %+v, want the request's policy", p)
	}

	// Empty is allow-all at startup (the create spec's words), but still behind a filter, so it
	// can be tightened later.
	empty := h.create(with(map[string]any{}))
	if p := h.p.service(empty.ID).EgressPolicy; p == nil || p.DefaultAction != "allow" {
		t.Fatalf("empty networkPolicy became %+v, want allow-all", p)
	}

	// No policy, no filter: nothing changes for a caller who did not ask.
	none := h.create(with(nil))
	if p := h.p.service(none.ID).EgressPolicy; p != nil {
		t.Fatalf("no networkPolicy produced %+v", p)
	}

	resp := h.do("POST", "/v1/sandboxes", with(map[string]any{"egress": []any{
		map[string]string{"action": "allow", "target": "bad target!"}}}), nil)
	if resp.StatusCode != 400 {
		t.Fatalf("an invalid rule = %d, want 400", resp.StatusCode)
	}

	h.errOf(resp)
}

// The SDK reaches a sandbox's policy through endpoints/18080 + /policy, as it would the
// upstream sidecar. That endpoint is the API's own listener, with a per-sandbox credential in
// the header the SDK sends there.
func TestEgressEndpointServesTheSidecarPolicy(t *testing.T) {
	h := newHarness(t, func(_ *harness, o *Options) { o.Key = "k" })
	key := []string{"OPEN-SANDBOX-API-KEY", "k"}

	var sb sandboxJSON
	h.do("POST", "/v1/sandboxes", minimalCreate(), &sb, key...)
	h.waitStateKeyed(sb.ID, key)

	var ep endpointJSON
	if resp := h.do("GET", "/v1/sandboxes/"+sb.ID+"/endpoints/18080", nil, &ep, key...); resp.StatusCode != 200 {
		t.Fatalf("endpoint 18080 = %d", resp.StatusCode)
	}

	tok := ep.Headers["OPENSANDBOX-EGRESS-AUTH"]
	host := strings.TrimPrefix(h.http.URL, "http://")

	if ep.Endpoint != host+"/v1/sandboxes/"+sb.ID+"/egress" || tok == "" {
		t.Fatalf("endpoint %+v, want this listener's /egress with a credential", ep)
	}

	sidecar := "/v1/sandboxes/" + sb.ID + "/egress/policy"

	if resp := h.do("GET", sidecar, nil, nil, "OPENSANDBOX-EGRESS-AUTH", tok); resp.StatusCode != 200 {
		t.Fatalf("GET /policy with the credential = %d", resp.StatusCode)
	}

	if resp := h.do("PATCH", sidecar, `[]`, nil, "OPENSANDBOX-EGRESS-AUTH", tok); resp.StatusCode != 200 {
		t.Fatalf("PATCH /policy with the credential = %d", resp.StatusCode)
	}

	for name, hdr := range map[string][]string{
		"none":              nil,
		"wrong":             {"OPENSANDBOX-EGRESS-AUTH", "nope"},
		"another sandbox's": {"OPENSANDBOX-EGRESS-AUTH", tok + "x"},
	} {
		if resp := h.do("GET", sidecar, nil, nil, hdr...); resp.StatusCode != 401 {
			t.Errorf("%s credential = %d, want 401", name, resp.StatusCode)
		}
	}

	if got := h.eg.seen(); got != "sidecar GET "+sb.ID+",sidecar PATCH "+sb.ID {
		t.Fatalf("the sidecar handler saw %q", got)
	}

	h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, nil, key...)

	if !strings.Contains(h.eg.seen(), "forget "+sb.ID) {
		t.Fatal("deleting a sandbox did not drop its saved policy")
	}
}
