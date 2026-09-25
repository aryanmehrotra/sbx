package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

func osbFleet(t *testing.T) *pausing {
	return &pausing{listingProvider: listingProvider{units: []provider.Unit{
		{Ref: "sbx-osb-aaaaaaaaaaaa-sandbox", Sandbox: "osb-aaaaaaaaaaaa", Service: "sandbox", Running: true,
			OSB: "sbx-serve:osb-", Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
		{Ref: "sbx-zopnight-mysql", Sandbox: "zopnight", Service: "mysql", Running: true,
			Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
	}}}
}

// The machine's own daemon is unscoped. Beside the daemon serving --osb-addr it used to adopt
// the API's containers as well: bind their ports, freeze or stop them on its own idle clock, and
// - knowing nothing of API pauses - thaw a Paused sandbox on the first connection. An API
// sandbox now has one daemon: the one serving the API, or one scoped to it on purpose.
func TestAnUnscopedDaemonWithoutTheAPILeavesAPISandboxesAlone(t *testing.T) {
	log.SetOutput(io.Discard)

	p := osbFleet(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := New(p, time.Millisecond, time.Second, time.Hour)
	d.discover(ctx)

	d.mu.Lock()
	_, api := d.units["sbx-osb-aaaaaaaaaaaa-sandbox"]
	_, classic := d.units["sbx-zopnight-mysql"]
	d.mu.Unlock()

	if api {
		t.Fatal("an unscoped daemon that does not serve the API adopted an API sandbox")
	}

	if !classic {
		t.Fatal("a classic sandbox was not adopted: the ownership fence must not touch them")
	}

	for _, u := range d.units {
		u.served = true
		u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())
	}

	d.reap(ctx)

	if got := p.seen(); strings.Contains(got, "osb-") {
		t.Fatalf("the reaper touched an API sandbox it does not own: %s", got)
	}

	control := map[string]struct {
		h    http.HandlerFunc
		body string
	}{
		"remove": {d.controlRemove, `{"sandbox":"osb-aaaaaaaaaaaa"}`},
		"sleep":  {d.controlSleep, `{"ref":"sbx-osb-aaaaaaaaaaaa-sandbox"}`},
		"wake":   {d.controlWake, `{"ref":"sbx-osb-aaaaaaaaaaaa-sandbox"}`},
		"limit":  {d.controlLimit, `{"ref":"sbx-osb-aaaaaaaaaaaa-sandbox","mem_bytes":10000000}`},
	}

	for name, c := range control {
		rec := httptest.NewRecorder()
		c.h(rec, httptest.NewRequest(http.MethodPost, "/v1/control/"+name, bytes.NewBufferString(c.body)))

		if rec.Code != http.StatusForbidden {
			t.Errorf("control %s on an API sandbox = %d, want 403", name, rec.Code)
		}
	}

	if got := p.seen(); strings.Contains(got, "osb-") {
		t.Fatalf("control reached an API sandbox it does not own: %s", got)
	}

	// Classic sandboxes are controlled as before.
	rec := httptest.NewRecorder()
	d.controlSleep(rec, httptest.NewRequest(http.MethodPost, "/v1/control/sleep",
		bytes.NewBufferString(`{"ref":"sbx-zopnight-mysql"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("control sleep on a classic sandbox = %d, want 200", rec.Code)
	}
}

// The other direction: the daemon serving the API owns its sandboxes, and so does one scoped to
// them on purpose (the conformance harness, a test daemon).
func TestTheAPIDaemonAndAScopedDaemonAdoptAPISandboxes(t *testing.T) {
	log.SetOutput(io.Discard)

	for name, set := range map[string]func(*daemon){
		"serves --osb-addr": func(d *daemon) { d.servesOSB = true },
		"--only osb-":       func(d *daemon) { d.scope = Scope{"osb-"} },
	} {
		p := osbFleet(t)
		d := New(p, time.Minute, time.Second, time.Hour)
		set(d)

		d.discover(context.Background())

		d.mu.Lock()
		_, api := d.units["sbx-osb-aaaaaaaaaaaa-sandbox"]
		d.mu.Unlock()

		if !api {
			t.Errorf("%s: the API sandbox was not adopted", name)
		}
	}
}

// Against a real engine: the label survives docker and comes back through List, and the
// adoption test reads it. Nothing here runs a daemon - an unscoped one on a shared engine would
// front and reap other people's sandboxes - so the container is made by hand, like the scope
// guard's, and only it is removed.
func TestDockerAPISandboxLabelIsReadBack(t *testing.T) {
	p := dockerOrSkip(t)
	log.SetOutput(io.Discard)

	suffix := fmt.Sprintf("%x", time.Now().UnixNano())[:12]
	sandbox := "osb-" + suffix
	name := "sbx-" + sandbox + "-sandbox"

	dockerCLI(t, "run", "-d", "--name", name,
		"--label", "sbx.sandbox="+sandbox, "--label", "sbx.slot=58", "--label", "sbx.service=sandbox",
		"--label", fmt.Sprintf("sbx.ports=%d:%d", freePort(t), freePort(t)),
		"--label", "sbx.osb=sbx-serve:test",
		"alpine:3.20", "sleep", "300")

	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	units, err := p.List(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}

	if len(units) != 1 || units[0].OSB != "sbx-serve:test" {
		t.Fatalf("List gave %+v, want one unit with OSB sbx-serve:test", units)
	}

	plain := New(p, time.Minute, time.Second, time.Hour)
	if plain.adopts(units[0]) {
		t.Fatal("an unscoped daemon without --osb-addr would adopt a labelled API sandbox")
	}

	plain.servesOSB = true
	if !plain.adopts(units[0]) {
		t.Fatal("the daemon serving the API would not adopt its own sandbox")
	}
}

func TestOSBIsRefusedOnFirecrackerAtStartup(t *testing.T) {
	for _, kind := range []string{"firecracker", "fc"} {
		if err := refuseOSBOnMicroVM(kind, "127.0.0.1:8080"); !errors.Is(err, provider.ErrOSBOnFirecracker) {
			t.Errorf("%s: %v", kind, err)
		}
	}

	if refuseOSBOnMicroVM("firecracker", "") != nil || refuseOSBOnMicroVM("docker", "127.0.0.1:8080") != nil {
		t.Error("refused something that works")
	}
}
