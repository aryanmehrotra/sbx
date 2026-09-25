package osb

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func (h *harness) waitTpl(id string, phases ...string) templateJSON {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		var tj templateJSON
		h.do("GET", "/v1/templates/"+id, nil, &tj)

		if slices.Contains(phases, tj.Status.Phase) {
			return tj
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("template %s stayed %s (%s), wanted %v", id, tj.Status.Phase, tj.Status.Message, phases)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) template(body map[string]any) templateJSON {
	h.t.Helper()

	var tj templateJSON
	if resp := h.do("POST", "/v1/templates", body, &tj); resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("create template = %d %+v", resp.StatusCode, h.errOf(resp))
	}

	return tj
}

func TestTemplateFromAnImageBuildsBySucceedingHere(t *testing.T) {
	h := newHarness(t)

	tj := h.template(map[string]any{"image": "python:3.11-slim", "publish": "s3://bucket/x",
		"entrypoint": []string{"python", "-m", "http.server"}, "metadata": map[string]string{"team": "ml"},
		"resourceLimits": map[string]string{"cpu": "1", "memory": "512Mi"}})

	if tj.Status.Phase != tplPending || !tplIDPattern.MatchString(tj.TemplateID) || tj.Format != "overlaybd" ||
		tj.Publish != "s3://bucket/x" {
		t.Fatalf("created = %+v", tj)
	}

	got := h.waitTpl(tj.TemplateID, tplSucceeded, tplFailed)
	if got.Status.Phase != tplSucceeded || got.Status.ManifestRef != "docker-image://python:3.11-slim" {
		t.Fatalf("built = %+v", got)
	}

	if !slices.Contains(h.p.pulled(), "python:3.11-slim") {
		t.Error("building a template must pull its image, so a create from it never waits on a registry")
	}

	var l listTemplatesJSON
	h.do("GET", "/v1/templates?metadata=team%3Dml", nil, &l)

	if len(l.Items) != 1 || l.Items[0].TemplateID != tj.TemplateID {
		t.Errorf("list by metadata = %+v", l)
	}

	h.do("GET", "/v1/templates?metadata=team%3Dweb", nil, &l)

	if len(l.Items) != 0 {
		t.Errorf("list by other metadata = %+v", l)
	}
}

func TestTemplateThatCannotRunHereFails(t *testing.T) {
	h := newHarness(t)

	h.p.mu.Lock()
	h.p.inspectErr = map[string]error{"broken:1": errors.New("no such image")}
	h.p.mu.Unlock()

	tj := h.template(map[string]any{"image": "broken:1"})

	if got := h.waitTpl(tj.TemplateID, tplSucceeded, tplFailed); got.Status.Phase != tplFailed ||
		!strings.Contains(got.Status.Message, "no such image") {
		t.Fatalf("got %+v", got.Status)
	}

	// Only Succeeded templates can create, and the refusal says why.
	resp := h.do("POST", "/v1/sandboxes", map[string]any{"templateId": tj.TemplateID, "timeout": 600}, nil)
	if e := h.errOf(resp); resp.StatusCode != http.StatusNotFound || e.Code != "FSB::TEMPLATE_NOT_FOUND" ||
		!strings.Contains(e.Message, "Failed") {
		t.Errorf("create from a Failed template = %d %+v", resp.StatusCode, e)
	}
}

func TestCreateTemplateRefusals(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name   string
		body   any
		status int
		says   string
	}{
		{"no image", map[string]any{"publish": "s3://x"}, 400, "image is required"},
		{"unknown field", map[string]any{"image": "x", "kernel": "y"}, 400, "kernel"},
		{"readiness", map[string]any{"image": "x", "readiness": map[string]any{"probe": "tcp://127.0.0.1:1"}}, 501, "readiness"},
		{"disk", map[string]any{"image": "x", "resourceLimits": map[string]string{"disk": "2Gi"}}, 400, "disk"},
		{"memory spelling", map[string]any{"image": "x", "resourceLimits": map[string]string{"memory": "512m"}}, 400, "512Mi"},
		{"format", map[string]any{"image": "x", "format": "qcow2"}, 400, "overlaybd"},
		{"missing snapshot", map[string]any{"image": "snap-000000000000"}, 404, "snapshot"},
	}

	for _, c := range cases {
		resp := h.do("POST", "/v1/templates", c.body, nil)
		if e := h.errOf(resp); resp.StatusCode != c.status || !strings.Contains(e.Message, c.says) {
			t.Errorf("%s: %d %+v, want %d saying %q", c.name, resp.StatusCode, e, c.status, c.says)
		}
	}

	if resp := h.do("GET", "/v1/templates/tpl-000000000000", nil, nil); resp.StatusCode != http.StatusNotFound ||
		h.errOf(resp).Code != "FSB::TEMPLATE_NOT_FOUND" {
		t.Errorf("GET missing = %d", resp.StatusCode)
	}
}

func TestCreateFromTemplate(t *testing.T) {
	h := newHarness(t)

	tj := h.template(map[string]any{"image": "python:3.11-slim",
		"entrypoint": []string{"python", "-m", "http.server"}, "resourceLimits": map[string]string{"cpu": "250m"}})
	h.waitTpl(tj.TemplateID, tplSucceeded)

	pullsBefore := len(h.p.pulled())

	refusals := []struct {
		body map[string]any
		says string
	}{
		{map[string]any{"templateId": tj.TemplateID}, "timeout is required"},
		{map[string]any{"templateId": tj.TemplateID, "timeout": 600, "entrypoint": []string{"sh"}}, "entrypoint"},
		{map[string]any{"templateId": tj.TemplateID, "timeout": 600, "env": map[string]string{"A": "b"}}, "env"},
		{map[string]any{"templateId": tj.TemplateID, "timeout": 600,
			"volumes": []any{map[string]any{"name": "v", "pvc": map[string]any{"claimName": "c"}, "mountPath": "/c"}}}, "volumes"},
	}

	for _, r := range refusals {
		resp := h.do("POST", "/v1/sandboxes", r.body, nil)
		if e := h.errOf(resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(e.Message, r.says) {
			t.Errorf("%v: %d %+v", r.body, resp.StatusCode, e)
		}
	}

	resp := h.do("POST", "/v1/sandboxes", map[string]any{"templateId": "tpl-000000000000", "timeout": 600}, nil)
	if resp.StatusCode != http.StatusNotFound || h.errOf(resp).Code != "FSB::TEMPLATE_NOT_FOUND" {
		t.Errorf("unknown template = %d", resp.StatusCode)
	}

	sb := h.create(map[string]any{"templateId": tj.TemplateID, "timeout": 600,
		"metadata": map[string]string{"run": "1"}})

	if sb.Status.State != stateRunning || !slices.Equal(sb.Entrypoint, []string{"python", "-m", "http.server"}) ||
		sb.Image.URI != "python:3.11-slim" || sb.ExpiresAt == nil || sb.Metadata["run"] != "1" {
		t.Fatalf("sandbox = %+v", sb)
	}

	if svc := h.p.service(sb.ID); svc.CPU != "0.25" {
		t.Errorf("cpu %q, want the template's 250m", svc.CPU)
	}

	if len(h.p.pulled()) != pullsBefore {
		t.Error("creating from a built template pulled again")
	}

	// Deleting the template leaves the sandbox made from it alone.
	if resp := h.do("DELETE", "/v1/templates/"+tj.TemplateID, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d", resp.StatusCode)
	}

	if resp := h.do("GET", "/v1/templates/"+tj.TemplateID, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d", resp.StatusCode)
	}

	if got := h.waitState(sb.ID, stateRunning); got.ID != sb.ID {
		t.Error("the sandbox went with its template")
	}
}

// A template over a snapshot: the golden-image idea, with sbx's own snapshot as the image.
func TestTemplateFromASnapshot(t *testing.T) {
	h := newHarness(t)
	_, sj := h.snapshotOf(nil)

	tj := h.template(map[string]any{"image": sj.ID})
	if got := h.waitTpl(tj.TemplateID, tplSucceeded, tplFailed); got.Status.Phase != tplSucceeded {
		t.Fatalf("template over a Ready snapshot = %+v", got.Status)
	}

	sb := h.create(map[string]any{"templateId": tj.TemplateID, "timeout": 600})
	if sb.Image.URI != "sbx-osb-snap:"+sj.ID || !slices.Equal(sb.Entrypoint, defaultRestoreEntrypoint) {
		t.Fatalf("sandbox = %+v", sb)
	}

	h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, nil)

	// The template still stands on the snapshot.
	resp := h.do("DELETE", "/v1/snapshots/"+sj.ID, nil, nil)
	if e := h.errOf(resp); resp.StatusCode != http.StatusConflict || !strings.Contains(e.Message, tj.TemplateID) {
		t.Fatalf("DELETE snapshot under a template = %d %+v", resp.StatusCode, e)
	}
}
