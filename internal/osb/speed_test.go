package osb

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// An image already on the engine is used as it is. `docker pull` of a present tag still asks the
// registry for the manifest, which measured 2.8-3.2 s per create on colima - most of a cold
// create - to learn nothing. Upstream's docker runtime makes the same choice (images.get, pull
// only on ImageNotFound).
func TestCreatePullsOnlyAMissingImage(t *testing.T) {
	h := newHarness(t)

	if got := h.create(minimalCreate()); got.Status.State != stateRunning {
		t.Fatalf("present image: state %s (%s), want Running", got.Status.State, got.Status.Message)
	}

	h.p.mu.Lock()
	pulls := slices.Clone(h.p.pulls)
	h.p.missing = map[string]bool{"node:22-slim": true}
	h.p.mu.Unlock()

	if len(pulls) != 0 {
		t.Fatalf("an image already present was pulled: %v", pulls)
	}

	body := minimalCreate()
	body["image"] = map[string]any{"uri": "node:22-slim"}

	if got := h.create(body); got.Status.State != stateRunning {
		t.Fatalf("missing image: state %s (%s), want Running after a pull", got.Status.State, got.Status.Message)
	}

	h.p.mu.Lock()
	defer h.p.mu.Unlock()

	if !slices.Equal(h.p.pulls, []string{"node:22-slim"}) {
		t.Fatalf("pulls %v, want exactly the missing image once", h.p.pulls)
	}
}

// A create that becomes ready quickly answers Running in the POST itself. The SDKs poll GET every
// two seconds until Running, so a sandbox that was ready at 300 ms but answered Pending cost the
// caller a full poll interval; answered Running, the SDK's first GET ends the wait.
func TestCreateAnswersRunningWhenReadyInTime(t *testing.T) {
	h := newHarness(t, func(_ *harness, o *Options) { o.CreateWait = 5 * time.Second })

	var created sandboxJSON
	resp := h.do("POST", "/v1/sandboxes", minimalCreate(), &created)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d, want 202 (the status code does not change, only the state)", resp.StatusCode)
	}

	if created.Status.State != stateRunning {
		t.Fatalf("create answered %s, want Running: it was ready well inside the wait", created.Status.State)
	}
}

// Past the wait, the POST answers Pending and provisioning carries on - a slow pull must not
// hold the request open until the client's own timeout fires.
func TestCreateAnswersPendingPastTheWait(t *testing.T) {
	h := newHarness(t, func(_ *harness, o *Options) { o.CreateWait = 50 * time.Millisecond })

	h.mu.Lock()
	h.pingErr = errNotYet
	h.mu.Unlock()

	start := time.Now()

	var created sandboxJSON
	h.do("POST", "/v1/sandboxes", minimalCreate(), &created)

	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("create took %s with a 50ms wait", took)
	}

	if created.Status.State != statePending {
		t.Fatalf("create answered %s before execd answered, want Pending", created.Status.State)
	}

	h.mu.Lock()
	h.pingErr = nil
	h.mu.Unlock()

	h.waitState(created.ID, stateRunning)
}

// The calls a client makes straight after a create - GET until Running, then the execd
// endpoint - ask docker about that one sandbox (an inspect, with the docker provider), or
// nothing: listing every container for each cost 15-25 ms apiece on colima and grew with every
// sandbox a burst added.
func TestGetAsksAboutOneSandboxAndEndpointNothing(t *testing.T) {
	h := newHarness(t)
	sb := h.create(minimalCreate())

	h.p.mu.Lock()
	h.p.lists = nil
	h.p.mu.Unlock()

	h.do("GET", "/v1/sandboxes/"+sb.ID, nil, nil)

	var ep endpointJSON
	if resp := h.do("GET", "/v1/sandboxes/"+sb.ID+"/endpoints/44772", nil, &ep); resp.StatusCode != http.StatusOK || ep.Endpoint == "" {
		t.Fatalf("endpoint = %d %+v", resp.StatusCode, ep)
	}

	h.p.mu.Lock()
	defer h.p.mu.Unlock()

	if !slices.Equal(h.p.lists, []string{sb.ID}) {
		t.Fatalf("lists %q, want one, of this sandbox only (the GET's); the endpoint needs none", h.p.lists)
	}
}

// pickingDocker is the fake with a slot picker, and a Create slow enough to overlap: what the
// docker provider looks like to a burst.
type pickingDocker struct {
	*fakeDocker

	inCreate, maxInCreate atomic.Int32
}

func (p *pickingDocker) PickSlot(taken map[int]bool) (int, error) {
	for i := range 128 {
		if !taken[i] {
			return i, nil
		}
	}

	return 0, errors.New("full")
}

func (p *pickingDocker) Create(ctx context.Context, sandbox string, slot, start int, svc string, s spec.Service,
	eps []provider.Endpoint, dir string, iso provider.Isolation) error {
	n := p.inCreate.Add(1)
	defer p.inCreate.Add(-1)

	for {
		m := p.maxInCreate.Load()
		if n <= m || p.maxInCreate.CompareAndSwap(m, n) {
			break
		}
	}

	time.Sleep(30 * time.Millisecond)

	return p.fakeDocker.Create(ctx, sandbox, slot, start, svc, s, eps, dir, iso)
}

// A burst of cold creates gets distinct slots without holding the slot lock through `docker
// run`: the lock covers the choice, an in-process reservation covers the gap until the
// container exists. And no more than dockerConcurrency runs are in flight at once.
func TestColdBurstGetsDistinctSlotsWithCreatesOverlapping(t *testing.T) {
	pd := &pickingDocker{fakeDocker: newFakeDocker()}

	h := newHarness(t, func(_ *harness, o *Options) {
		o.Provider = pd
		o.DockerConcurrency = 4
	})

	const n = 12

	var wg sync.WaitGroup

	ids := make(chan string, n)

	for range n {
		wg.Add(1)

		go func() {
			defer wg.Done()

			var sb sandboxJSON
			h.do("POST", "/v1/sandboxes", minimalCreate(), &sb)
			ids <- sb.ID
		}()
	}

	wg.Wait()
	close(ids)

	for id := range ids {
		h.waitState(id, stateRunning)
	}

	pd.mu.Lock()
	slots := map[int]string{}
	for sb, u := range pd.units {
		if other, dup := slots[u.Slot]; dup {
			t.Errorf("slot %d given to both %s and %s", u.Slot, other, sb)
		}

		slots[u.Slot] = sb
	}
	pd.mu.Unlock()

	if len(slots) != n {
		t.Fatalf("%d distinct slots for %d creates", len(slots), n)
	}

	if m := pd.maxInCreate.Load(); m < 2 || m > 4 {
		t.Fatalf("at most %d creates overlapped; want 2..4 (overlapping, and bounded by 4)", m)
	}
}
