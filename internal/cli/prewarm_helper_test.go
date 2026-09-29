package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A puller whose backend runs a helper image of its own, as docker's volume copy does.
type helperPuller struct{ fakePuller }

func (h *helperPuller) HelperImages(n provider.HelperNeeds) []provider.Helper {
	var out []provider.Helper

	if n.Volumes {
		out = append(out, provider.Helper{Image: "alpine:3", For: "snapshot and fork copy volumes with it"})
	}

	if n.Egress {
		out = append(out, provider.Helper{Image: "golang:1.26-alpine", For: "the egress filter is built with it"},
			provider.Helper{Image: "alpine:3.20", For: "the egress filter runs on it"})
	}

	return out
}

// `sbx prewarm --spec F` pulled the spec's images but not the helper snapshot and fork copy a
// volume with, so the first fork after a "warm" CI step was a 95 s pull. Prewarm pulls it too
// and says so, and pulls it only when asked: IMAGE... means exactly those images.
func TestPrewarmPullsTheVolumeCopyHelper(t *testing.T) {
	h := &helperPuller{fakePuller{present: map[string]bool{}}}

	var out bytes.Buffer
	if err := Prewarm(context.Background(), h, &out, []string{"redis:7"}, provider.HelperNeeds{Volumes: true}); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}

	if !slices.Contains(h.pulled, "alpine:3") {
		t.Fatalf("the helper was not pulled: %v", h.pulled)
	}

	if !strings.Contains(out.String(), "alpine:3") || !strings.Contains(out.String(), "helper") {
		t.Errorf("the summary does not say the helper was pulled:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "2 pulled") {
		t.Errorf("the count leaves the helper out:\n%s", out.String())
	}

	// Already there: counted as present, not pulled again.
	h.pulled = nil
	h.present = map[string]bool{"redis:7": true, "alpine:3": true}
	out.Reset()

	if err := Prewarm(context.Background(), h, &out, []string{"redis:7"}, provider.HelperNeeds{Volumes: true}); err != nil {
		t.Fatal(err)
	}

	if len(h.pulled) != 0 || !strings.Contains(out.String(), "0 pulled, 2 already present") {
		t.Errorf("pulled %v; output:\n%s", h.pulled, out.String())
	}

	// Not asked for helpers: only the named images.
	h.pulled = nil
	h.present = map[string]bool{}

	if err := Prewarm(context.Background(), h, &out, []string{"redis:7"}, provider.HelperNeeds{}); err != nil {
		t.Fatal(err)
	}

	if slices.Contains(h.pulled, "alpine:3") {
		t.Errorf("the helper was pulled although not asked for: %v", h.pulled)
	}
}

// The helper is named once even when the images already include it.
func TestPrewarmDoesNotPullTheHelperTwice(t *testing.T) {
	h := &helperPuller{fakePuller{present: map[string]bool{}}}

	var out bytes.Buffer
	if err := Prewarm(context.Background(), h, &out, []string{"alpine:3"}, provider.HelperNeeds{Volumes: true}); err != nil {
		t.Fatal(err)
	}

	if len(h.pulled) != 1 {
		t.Errorf("pulled %v, want alpine:3 once", h.pulled)
	}
}

// A filtered spec's prewarm pulls the filter's build and runtime images and says what each is for.
func TestPrewarmPullsTheEgressFilterImages(t *testing.T) {
	h := &helperPuller{fakePuller{present: map[string]bool{}}}

	var out bytes.Buffer
	if err := Prewarm(context.Background(), h, &out, []string{"app:1"}, provider.HelperNeeds{Egress: true}); err != nil {
		t.Fatal(err)
	}

	for _, img := range []string{"golang:1.26-alpine", "alpine:3.20"} {
		if !slices.Contains(h.pulled, img) {
			t.Errorf("%s was not pulled: %v", img, h.pulled)
		}
	}

	if slices.Contains(h.pulled, "alpine:3") {
		t.Errorf("the volume helper was pulled for a spec that only filters: %v", h.pulled)
	}

	if !strings.Contains(out.String(), "egress filter") || !strings.Contains(out.String(), "3 pulled") {
		t.Errorf("the summary does not report the filter's images:\n%s", out.String())
	}
}
