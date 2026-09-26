package osb

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// fakeParkVM is fakeVM with the microVM warm pool: members are parked and claimed through the
// provider, and a claim re-keys the "execd" of the member, whose token is tracked per ref.
type fakeParkVM struct {
	fakeVM
	pk *parkLog
}

type parkLog struct {
	mu       sync.Mutex
	parks    []string          // "ref asleep" or "ref frozen"
	claims   []parkClaim       // every claim asked for
	tokens   map[string]string // what each member's execd answers, by ref
	claimErr error

	// gate, when set, holds every Park until it is closed; inPark and maxInPark count them.
	gate      chan struct{}
	inPark    int
	maxInPark int
}

type parkClaim struct {
	ref, token string
	env        map[string]string
}

func (f fakeParkVM) Park(ctx context.Context, ref string, frozen bool) error {
	f.pk.mu.Lock()
	f.pk.inPark++
	f.pk.maxInPark = max(f.pk.maxInPark, f.pk.inPark)
	gate := f.pk.gate
	f.pk.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}

	f.pk.mu.Lock()
	defer f.pk.mu.Unlock()

	f.pk.inPark--

	mode := "asleep"
	if frozen {
		mode = "frozen"
	}

	f.pk.parks = append(f.pk.parks, ref+" "+mode)

	return nil
}

func (f fakeParkVM) Claim(_ context.Context, ref, token string, env map[string]string) error {
	f.pk.mu.Lock()
	defer f.pk.mu.Unlock()

	f.pk.claims = append(f.pk.claims, parkClaim{ref, token, maps.Clone(env)})

	if f.pk.claimErr != nil {
		return f.pk.claimErr
	}

	f.pk.tokens[ref] = token

	return nil
}

func (p *parkLog) all() ([]string, []parkClaim) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return slices.Clone(p.parks), slices.Clone(p.claims)
}

var _ provider.PoolParker = fakeParkVM{}

// vmPoolHarness is a server on a RunsAgent provider with one pool of python:3.11-slim, running.
// Claim is a seam that fails the test: a microVM member is never claimed over execd's HTTP.
func vmPoolHarness(t *testing.T, size int, pk *parkLog, opts ...option) *harness {
	t.Helper()

	pk.tokens = map[string]string{}

	h := newHarness(t, append([]option{func(h *harness, o *Options) {
		o.Provider = fakeParkVM{fakeVM{h.p}, pk}
		o.Pools = []PoolSpec{{Image: "python:3.11-slim", Size: size}}
		o.Claim = func(context.Context, string, string, string, map[string]string) error {
			t.Error("a microVM member was claimed through POST /sbx/claim, not the provider's re-key")
			return errors.New("wrong path")
		}
	}}, opts...)...)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go h.srv.Run(ctx)

	return h
}

func (h *harness) members() []string {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()

	return slices.Sorted(maps.Keys(h.p.units))
}

func (h *harness) held(id string) bool {
	h.rt.mu.Lock()
	defer h.rt.mu.Unlock()

	return h.rt.held[id]
}

// Members are parked asleep, invisible and held; a matching create is served Running from one,
// re-keyed through the provider with the API's fresh token and the caller's env - not the token
// the member booted with - and the pool refills behind it.
func TestVMPoolParksAsleepAndClaimsWithTheAPIsToken(t *testing.T) {
	pk := &parkLog{}
	h := vmPoolHarness(t, 2, pk)
	h.poolReady(2)

	parks, _ := pk.all()
	if len(parks) != 2 || !strings.HasSuffix(parks[0], " asleep") || !strings.HasSuffix(parks[1], " asleep") {
		t.Fatalf("parks = %v, want both members asleep", parks)
	}

	var page listJSON
	h.do("GET", "/v1/sandboxes", nil, &page)

	if len(page.Items) != 0 {
		t.Fatalf("list shows %d sandboxes: members are visible", len(page.Items))
	}

	for _, id := range h.members() {
		if !h.held(id) {
			t.Fatalf("member %s is not held: a connection to it would wake it with the member's token", id)
		}

		if resp := h.do("GET", "/v1/sandboxes/"+id, nil, nil); resp.StatusCode != 404 {
			t.Fatalf("GET of an unclaimed member = %d", resp.StatusCode)
		}
	}

	if strings.Contains(h.rt.seen(), "freeze") {
		t.Fatalf("an asleep member was frozen through the daemon: %s", h.rt.seen())
	}

	body := sdkCreate()
	body["env"] = map[string]string{"FOO": "bar"}

	var created sandboxJSON
	if resp := h.do("POST", "/v1/sandboxes", body, &created); resp.StatusCode != 202 || created.Status.State != stateRunning {
		t.Fatalf("create = %d %s, want 202 Running from the pool", resp.StatusCode, created.Status.State)
	}

	_, claims := pk.all()
	if len(claims) != 1 || claims[0].env["FOO"] != "bar" {
		t.Fatalf("claims = %+v, want one, with the caller's env", claims)
	}

	boot := h.p.service(created.ID).Env[tokenEnv]

	var ep endpointJSON
	h.do("GET", "/v1/sandboxes/"+created.ID+"/endpoints/44772", nil, &ep)

	switch tok := ep.Headers[tokenHeader]; {
	case tok == "" || tok != claims[0].token:
		t.Fatalf("endpoint token %q, claim token %q: the caller must get what execd was re-keyed to", tok, claims[0].token)
	case tok == boot:
		t.Fatal("the caller was handed the token the member booted with")
	case pk.tokens[claims[0].ref] != tok:
		t.Fatalf("the member's execd answers %q, not the caller's token", pk.tokens[claims[0].ref])
	}

	if h.held(created.ID) {
		t.Fatal("a claimed sandbox is still held: its caller's connections would be refused")
	}

	if !strings.Contains(h.rt.seen(), "unpin "+created.ID) {
		t.Fatalf("the claimed member was not unpinned: %s", h.rt.seen())
	}

	h.do("GET", "/v1/sandboxes", nil, &page)

	if len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatalf("list after the claim = %+v", page.Items)
	}

	h.poolReady(2)

	if n := h.p.count(); n != 3 {
		t.Fatalf("%d VMs, want the claimed one plus 2 members", n)
	}
}

// --osb-pool-freeze parks members paused in RAM; the claim is the same provider re-key.
func TestVMPoolFreezeParksFrozen(t *testing.T) {
	pk := &parkLog{}
	h := vmPoolHarness(t, 1, pk, func(_ *harness, o *Options) { o.PoolFreeze = true })
	h.poolReady(1)

	if parks, _ := pk.all(); len(parks) != 1 || !strings.HasSuffix(parks[0], " frozen") {
		t.Fatalf("parks = %v, want one frozen member", parks)
	}

	var created sandboxJSON
	h.do("POST", "/v1/sandboxes", sdkCreate(), &created)

	if _, claims := pk.all(); created.Status.State != stateRunning || len(claims) != 1 {
		t.Fatalf("frozen claim: %s, claims %+v", created.Status.State, claims)
	}
}

// A member whose claim fails is discarded, never handed out; the create goes cold and succeeds.
func TestVMPoolFailedClaimDiscardsAndGoesCold(t *testing.T) {
	pk := &parkLog{claimErr: errors.New("execd refused the re-key")}
	h := vmPoolHarness(t, 1, pk)
	h.poolReady(1)

	member := h.members()[0]

	got := h.create(sdkCreate())
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
			t.Fatal("the member whose re-key failed was never removed")
		}

		time.Sleep(10 * time.Millisecond)
	}

	if resp := h.do("GET", "/v1/sandboxes/"+member, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("the failed member is visible: %d", resp.StatusCode)
	}
}

// A request the pool cannot serve - here a network policy, which members are made without - is
// never claimed; the cold path serves it.
func TestVMPoolLeavesANetworkPolicyToTheColdPath(t *testing.T) {
	pk := &parkLog{}
	h := vmPoolHarness(t, 1, pk)
	h.poolReady(1)

	b := sdkCreate()
	b["networkPolicy"] = map[string]any{"defaultAction": "deny", "egress": []any{
		map[string]any{"action": "allow", "target": "pypi.org"}}}

	var created sandboxJSON
	if resp := h.do("POST", "/v1/sandboxes", b, &created); resp.StatusCode != 202 {
		t.Fatalf("create with a policy = %d %+v", resp.StatusCode, h.errOf(resp))
	}

	if _, claims := pk.all(); len(claims) != 0 {
		t.Fatalf("a create with a network policy was served from the pool: %+v", claims)
	}
}

// Unclaimed members go when the server does; claimed sandboxes stay.
func TestVMPoolCloseRemovesUnclaimedMembers(t *testing.T) {
	pk := &parkLog{}
	h := vmPoolHarness(t, 2, pk)
	h.poolReady(2)

	var created sandboxJSON
	h.do("POST", "/v1/sandboxes", sdkCreate(), &created)
	h.poolReady(2)

	h.srv.Close()

	if left := h.members(); len(left) != 1 || left[0] != created.ID {
		t.Fatalf("after Close: %v, want only the claimed %s", left, created.ID)
	}
}

// Members are made - booted and parked - no more than PoolConcurrency at once.
func TestVMPoolFillIsBounded(t *testing.T) {
	pk := &parkLog{gate: make(chan struct{})}
	h := vmPoolHarness(t, 5, pk, func(_ *harness, o *Options) { o.PoolConcurrency = 2 })

	deadline := time.Now().Add(5 * time.Second)

	for {
		pk.mu.Lock()
		n := pk.inPark
		pk.mu.Unlock()

		if n == 2 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("%d parks in flight, want 2", n)
		}

		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(50 * time.Millisecond) // room for a third, if the bound were broken

	close(pk.gate)
	h.poolReady(5)

	pk.mu.Lock()
	defer pk.mu.Unlock()

	if pk.maxInPark != 2 {
		t.Fatalf("%d members parked at once, want at most 2", pk.maxInPark)
	}
}
