package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A second daemon on the same docker engine adopts every container labelled sbx.sandbox, so
// without a scope it fronts - and after --idle, SLEEPS - somebody else's live stack. `--only`
// is the fence: outside it, the daemon must not so much as bind a port.
func TestOutOfScopeSandboxesAreNeverAdopted(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &pausing{listingProvider: listingProvider{units: []provider.Unit{
		{Ref: "sbx-osb-aaaaaaaaaaaa-sandbox", Sandbox: "osb-aaaaaaaaaaaa", Service: "sandbox", Running: true,
			Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
		{Ref: "sbx-zopnight-mysql", Sandbox: "zopnight", Service: "mysql", Running: true,
			Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
	}}}

	scope, err := ParseScope([]string{"osb-"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := New(p, time.Millisecond, time.Second, time.Hour)
	d.scope = scope
	d.discover(ctx)

	d.mu.Lock()
	_, adopted := d.units["sbx-zopnight-mysql"]
	_, ours := d.units["sbx-osb-aaaaaaaaaaaa-sandbox"]
	d.mu.Unlock()

	if adopted {
		t.Fatal("a sandbox outside --only was adopted: this daemon would front and sleep it")
	}

	if !ours {
		t.Fatal("the in-scope sandbox was not adopted")
	}

	// Idle far past the window: the reaper may act on what it owns and nothing else.
	for _, u := range d.units {
		u.served = true
		u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())
	}

	d.reap(ctx)

	if got := p.seen(); strings.Contains(got, "zopnight") {
		t.Fatalf("the reaper touched an out-of-scope sandbox: %s", got)
	}

	// Control is fenced the same way: remove by name, and act by ref.
	for path, body := range map[string]string{
		"/v1/control/remove": `{"sandbox":"zopnight"}`,
		"/v1/control/sleep":  `{"ref":"sbx-zopnight-mysql"}`,
		"/v1/control/wake":   `{"ref":"sbx-zopnight-mysql"}`,
		"/v1/control/limit":  `{"ref":"sbx-zopnight-mysql","mem_bytes":10000000}`,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))

		switch path {
		case "/v1/control/remove":
			d.controlRemove(rec, req)
		case "/v1/control/sleep":
			d.controlSleep(rec, req)
		case "/v1/control/wake":
			d.controlWake(rec, req)
		case "/v1/control/limit":
			d.controlLimit(rec, req)
		}

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s on an out-of-scope sandbox = %d, want 403", path, rec.Code)
		}
	}

	if got := p.seen(); strings.Contains(got, "zopnight") {
		t.Fatalf("control reached an out-of-scope sandbox: %s", got)
	}
}

func TestScopePatterns(t *testing.T) {
	s, err := ParseScope([]string{"osb-", "ci-*-db,pr-[0-9]*"})
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]bool{
		"osb-123":    true,
		"ci-7-db":    true,
		"pr-42":      true,
		"zopnight":   false,
		"xosb-1":     false,
		"ci-7-cache": false,
	} {
		if got := s.Match(name); got != want {
			t.Errorf("Match(%q) = %v, want %v", name, got, want)
		}
	}

	var all Scope
	if !all.Match("anything") {
		t.Fatal("the default scope must be everything, as before")
	}

	if _, err := ParseScope([]string{"bad["}); err == nil {
		t.Fatal("a malformed glob was accepted")
	}
}
