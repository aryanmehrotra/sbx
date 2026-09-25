package osb

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// claimLog records what the pool asked execd to do.
type claimLog struct {
	mu    sync.Mutex
	calls []claimCall
	err   error
}

type claimCall struct {
	addr, oldToken, newToken string
	env                      map[string]string
}

func (c *claimLog) claim(_ context.Context, addr, oldTok, newTok string, env map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls = append(c.calls, claimCall{addr, oldTok, newTok, maps.Clone(env)})

	return c.err
}

func (c *claimLog) all() []claimCall {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.calls)
}

// poolHarness is a server with one pool of python:3.11-slim, running.
func poolHarness(t *testing.T, size int, cl *claimLog, opts ...option) *harness {
	t.Helper()

	h := newHarness(t, append([]option{func(_ *harness, o *Options) {
		o.Pools = []PoolSpec{{Image: "python:3.11-slim", Size: size}}
		o.Claim = cl.claim
	}}, opts...)...)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go h.srv.Run(ctx)

	return h
}

// sdkCreate is what the SDK sends for create(image) with nothing else set.
func sdkCreate() map[string]any {
	return map[string]any{
		"image":          map[string]any{"uri": "python:3.11-slim"},
		"entrypoint":     []string{"tail", "-f", "/dev/null"},
		"resourceLimits": map[string]string{"cpu": "1", "memory": "2Gi"},
		"timeout":        600,
	}
}

func (h *harness) poolReady(want int) {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		var st []poolStatus
		h.do("GET", "/sbx/v1/pool", nil, &st)

		if len(st) == 1 && st[0].Ready == want && st[0].Filling == 0 {
			return
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("pool never reached %d ready: %+v", want, st)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fakeDocker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.units)
}

func TestPoolServesAMatchingCreateInvisiblyAndRefills(t *testing.T) {
	cl := &claimLog{}
	h := poolHarness(t, 2, cl)
	h.poolReady(2)

	// Members exist as containers, frozen, and as nothing at all through the API.
	if n := h.p.count(); n != 2 {
		t.Fatalf("%d containers, want the 2 members", n)
	}

	var page listJSON
	h.do("GET", "/v1/sandboxes", nil, &page)

	if len(page.Items) != 0 {
		t.Fatalf("list shows %d sandboxes before any create: pool members are visible", len(page.Items))
	}

	h.p.mu.Lock()
	var member string
	for id := range h.p.units {
		member = id
	}
	h.p.mu.Unlock()

	if resp := h.do("GET", "/v1/sandboxes/"+member, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET of an unclaimed member = %d, want 404", resp.StatusCode)
	}

	if strings.Count(h.rt.seen(), "pin osb-") != 2 || strings.Contains(h.rt.seen(), "freeze") {
		t.Fatalf("members were not pinned running: runtime saw %s", h.rt.seen())
	}

	body := sdkCreate()
	body["env"] = map[string]string{"FOO": "bar"}
	body["metadata"] = map[string]string{"team": "ml"}

	var created sandboxJSON
	resp := h.do("POST", "/v1/sandboxes", body, &created)

	if resp.StatusCode != http.StatusAccepted || created.Status.State != stateRunning {
		t.Fatalf("create = %d %s, want 202 Running straight from the pool", resp.StatusCode, created.Status.State)
	}

	if h.p.count() < 2 {
		t.Fatalf("a claimed create made no container of its own - fine - but the pool lost one: %d", h.p.count())
	}

	calls := cl.all()
	if len(calls) != 1 || calls[0].env["FOO"] != "bar" || calls[0].newToken == calls[0].oldToken {
		t.Fatalf("claim calls %+v, want one, carrying the env and a new token", calls)
	}

	if !strings.Contains(h.rt.seen(), "unpin "+created.ID) || strings.Contains(h.rt.seen(), "thaw") {
		t.Fatalf("the claimed member was not unpinned, or was thawed though never frozen: %s", h.rt.seen())
	}

	// The caller's sandbox, with the caller's token on its endpoint and nothing of the pool's.
	var ep endpointJSON
	h.do("GET", "/v1/sandboxes/"+created.ID+"/endpoints/44772", nil, &ep)

	if ep.Headers[tokenHeader] != calls[0].newToken {
		t.Fatalf("endpoint token %q, want the one execd was re-keyed to", ep.Headers[tokenHeader])
	}

	var got sandboxJSON
	h.do("GET", "/v1/sandboxes/"+created.ID, nil, &got)

	if got.Status.State != stateRunning || got.Metadata["team"] != "ml" || got.ExpiresAt == nil {
		t.Fatalf("GET after a claim: %+v", got)
	}

	h.do("GET", "/v1/sandboxes", nil, &page)

	if len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatalf("list after one create: %+v, want just that one", page.Items)
	}

	// Refilled behind the claim.
	h.poolReady(2)

	if n := h.p.count(); n != 3 {
		t.Fatalf("%d containers, want the claimed one plus 2 members", n)
	}
}

func TestPoolIsNotUsedForADifferentShapeOrWhenTurnedOff(t *testing.T) {
	cl := &claimLog{}
	h := poolHarness(t, 1, cl)
	h.poolReady(1)

	other := sdkCreate()
	other["resourceLimits"] = map[string]string{"cpu": "1", "memory": "1Gi"}

	if got := h.create(other); got.Status.State != stateRunning {
		t.Fatalf("cold create: %s", got.Status.State)
	}

	off := sdkCreate()
	off["extensions"] = map[string]string{"sbx.pool": "off"}

	if got := h.create(off); got.Status.State != stateRunning {
		t.Fatalf("cold create: %s", got.Status.State)
	}

	if calls := cl.all(); len(calls) != 0 {
		t.Fatalf("the pool served %d creates it should not have", len(calls))
	}

	bad := sdkCreate()
	bad["extensions"] = map[string]string{"sbx.pool": "maybe"}

	if resp := h.do("POST", "/v1/sandboxes", bad, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("sbx.pool=maybe = %d, want 400", resp.StatusCode)
	}
}

func TestAMemberThatCannotBeClaimedIsDiscardedAndTheCreateStillSucceeds(t *testing.T) {
	cl := &claimLog{err: errors.New("execd said no")}
	h := poolHarness(t, 1, cl)
	h.poolReady(1)

	h.p.mu.Lock()
	var member string
	for id := range h.p.units {
		member = id
	}
	h.p.mu.Unlock()

	withEnv := sdkCreate()
	withEnv["env"] = map[string]string{"A": "b"} // env is what makes a claim call execd

	got := h.create(withEnv)
	if got.Status.State != stateRunning || got.ID == member {
		t.Fatalf("create after a failed claim: %s %s, want a cold Running sandbox", got.ID, got.Status.State)
	}

	deadline := time.Now().Add(5 * time.Second)

	for {
		h.p.mu.Lock()
		removed := slices.Contains(h.p.removed, member)
		h.p.mu.Unlock()

		if removed {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("the member that failed its claim was never removed")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func TestCloseRemovesUnclaimedMembersOnly(t *testing.T) {
	cl := &claimLog{}
	h := poolHarness(t, 2, cl)
	h.poolReady(2)

	var created sandboxJSON
	h.do("POST", "/v1/sandboxes", sdkCreate(), &created)
	h.poolReady(2)

	h.srv.Close()

	h.p.mu.Lock()
	defer h.p.mu.Unlock()

	if len(h.p.units) != 1 || h.p.units[created.ID] == nil {
		t.Fatalf("after Close: %d containers left (%v), want only the claimed %s", len(h.p.units),
			slices.Collect(maps.Keys(h.p.units)), created.ID)
	}
}

func TestParsePool(t *testing.T) {
	for in, want := range map[string]PoolSpec{
		"node:22-slim":               {"node:22-slim", defaultPoolSize},
		"node:22-slim=10":            {"node:22-slim", 10},
		"ghcr.io/a/b@sha256:abc=3":   {"ghcr.io/a/b@sha256:abc", 3},
		"localhost:5000/img:tag = 2": {"localhost:5000/img:tag", 2},
	} {
		got, err := ParsePool(in)
		if err != nil || got != want {
			t.Errorf("ParsePool(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}

	for _, in := range []string{"", "=3", "node=0", "node=-1", "node=x"} {
		if _, err := ParsePool(in); err == nil {
			t.Errorf("ParsePool(%q) accepted", in)
		}
	}
}

// With PoolFreeze a member waits frozen and held, and the claim thaws it first.
func TestFrozenPoolThawsOnClaim(t *testing.T) {
	cl := &claimLog{}
	h := poolHarness(t, 1, cl, func(_ *harness, o *Options) { o.PoolFreeze = true })
	h.poolReady(1)

	if !strings.Contains(h.rt.seen(), "freeze osb-") {
		t.Fatalf("member not frozen: %s", h.rt.seen())
	}

	withEnv := sdkCreate()
	withEnv["env"] = map[string]string{"A": "b"}

	var created sandboxJSON
	h.do("POST", "/v1/sandboxes", withEnv, &created)

	seen := h.rt.seen()
	thaw := strings.Index(seen, "thaw "+created.ID)

	if created.Status.State != stateRunning || thaw < 0 || len(cl.all()) != 1 {
		t.Fatalf("frozen claim: state %s, runtime %s, claims %d", created.Status.State, seen, len(cl.all()))
	}
}

// A claim with no env still re-keys execd: the member's token predates its caller, and only a
// token minted for the request is known to be nobody else's.
func TestClaimWithoutEnvStillRekeys(t *testing.T) {
	cl := &claimLog{}
	h := poolHarness(t, 1, cl)
	h.poolReady(1)

	var created sandboxJSON
	h.do("POST", "/v1/sandboxes", sdkCreate(), &created)

	if created.Status.State != stateRunning {
		t.Fatalf("create: %s", created.Status.State)
	}

	calls := cl.all()
	if len(calls) != 1 || calls[0].newToken == calls[0].oldToken || len(calls[0].env) != 0 {
		t.Fatalf("claims %+v, want one re-key with no env", calls)
	}

	var ep endpointJSON
	h.do("GET", "/v1/sandboxes/"+created.ID+"/endpoints/44772", nil, &ep)

	if ep.Headers[tokenHeader] != calls[0].newToken {
		t.Fatalf("endpoint token %q, want the re-keyed one", ep.Headers[tokenHeader])
	}
}

// Only what a member can honour is served from the pool, and that is decided by a whitelist of
// request fields, not a list of the ones known to matter: a field this code has never heard of
// (volumes, a template, a claim added upstream later) must send the create down the cold path,
// never be dropped by a member that was made without it.
func TestPoolServesOnlyWhitelistedRequestFields(t *testing.T) {
	for name, set := range map[string]func(map[string]any){
		"volumes": func(b map[string]any) {
			b["volumes"] = []any{map[string]any{"name": "v", "host": map[string]any{"path": "/tmp"}}}
		},
		"snapshotId":    func(b map[string]any) { b["snapshotId"] = "snap-1" },
		"templateId":    func(b map[string]any) { b["templateId"] = "tpl-1" },
		"networkPolicy": func(b map[string]any) { b["networkPolicy"] = map[string]any{"defaultAction": "deny"} },
		"unknown field": func(b map[string]any) { b["claims"] = []any{"x"} },
		"extension":     func(b map[string]any) { b["extensions"] = map[string]string{"sbx.ports": "8080"} },
	} {
		t.Run(name, func(t *testing.T) {
			cl := &claimLog{}
			h := poolHarness(t, 1, cl)
			h.poolReady(1)

			body := sdkCreate()
			body["env"] = map[string]string{"A": "b"}
			set(body)

			var got sandboxJSON
			h.do("POST", "/v1/sandboxes", body, &got)

			if n := len(cl.all()); n != 0 {
				t.Fatalf("a create with %s was served from the pool (%d claims)", name, n)
			}
		})
	}
}

