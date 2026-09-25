package daemon

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A sandbox paused through the OpenSandbox API is frozen with its memory kept. The control
// API's sleep would docker-stop it and lose that memory; its wake would thaw it behind the API,
// which still reports Paused. Both refuse and point at the API's resume instead.
func TestControlVerbsRefuseAnAPIPausedSandbox(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &pausing{}
	u := newUnit("osb-0123456789ab", "sandbox", "sbx-osb-0123456789ab-sandbox", "i1", "sbx-osb-0123456789ab-sandbox", nil, false)

	d := &daemon{provider: p, units: map[string]*unit{u.name: u}}
	d.Hold("osb-0123456789ab", true)

	for path, h := range map[string]http.HandlerFunc{
		"/v1/control/sleep": d.controlSleep,
		"/v1/control/wake":  d.controlWake,
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"ref":"sbx-osb-0123456789ab-sandbox"}`)))

		if rec.Code != http.StatusConflict {
			t.Errorf("%s on an API-paused sandbox = %d, want 409", path, rec.Code)
		}

		if !strings.Contains(rec.Body.String(), "/v1/sandboxes/osb-0123456789ab/resume") {
			t.Errorf("%s refusal does not point at the API resume: %s", path, rec.Body.String())
		}
	}

	if got := p.seen(); got != "" {
		t.Fatalf("control reached the provider for a held sandbox: %s", got)
	}

	// Released, the same verbs work again - the refusal is the hold, not the sandbox.
	d.Hold("osb-0123456789ab", false)

	rec := httptest.NewRecorder()
	d.controlSleep(rec, httptest.NewRequest(http.MethodPost, "/v1/control/sleep",
		bytes.NewBufferString(`{"ref":"sbx-osb-0123456789ab-sandbox"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("sleep after resume = %d, want 200", rec.Code)
	}
}
