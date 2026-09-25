package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A sandbox paused through the API must stay paused across a daemon restart. The API used to
// re-assert its holds from Run, in a goroutine, while the daemon's first discovery bound the
// listeners - so for that window a connection found an unheld unit and thawed a sandbox the API
// still reported Paused. The holds are now in place before openSandboxAPI returns, which is
// before the daemon discovers anything.
func TestPersistedPauseHoldsBeforeTheFirstDiscovery(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Setenv("HOME", t.TempDir())

	const id = "osb-0123456789ab"

	dir := filepath.Join(os.Getenv("HOME"), ".sbx", "osb")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	rec := `{"id":"` + id + `","image":"alpine","entrypoint":["sleep","1"],"ports":[44772],` +
		`"token":"t","createdAt":"2026-09-26T00:00:00Z","state":"Paused",` +
		`"lastTransitionAt":"2026-09-26T00:00:00Z","pausedByApi":true}`
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &listingProvider{units: []provider.Unit{{
		Ref: "sbx-" + id + "-sandbox", Sandbox: id, Service: "sandbox", Paused: true,
		Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}},
	}}}

	d := New(p, time.Minute, time.Second, time.Hour)

	api, ln, err := openAPI(d, "127.0.0.1:0", "k")
	if err != nil {
		t.Fatal(err)
	}

	defer ln.Close()
	defer api.Close()

	// No api.Run: this is the moment d.run starts discovering, before the API's goroutine has
	// had any chance to do anything.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.discover(ctx)

	d.mu.Lock()
	u := d.units["sbx-"+id+"-sandbox"]
	d.mu.Unlock()

	if u == nil {
		t.Fatal("the paused sandbox was not discovered")
	}

	if !u.isHeld() {
		t.Fatal("the first discovery fronted an API-paused sandbox unheld: a connection now would thaw it")
	}
}
