package osb

import (
	"net/http"
	"slices"
	"testing"
	"time"
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
