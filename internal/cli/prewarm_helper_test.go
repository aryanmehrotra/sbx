package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

// A puller whose backend runs a helper image of its own, as docker's volume copy does.
type helperPuller struct{ fakePuller }

func (h *helperPuller) HelperImages() []string { return []string{"alpine:3"} }

// `sbx prewarm --spec F` pulled the spec's images but not the helper snapshot and fork copy a
// volume with, so the first fork after a "warm" CI step was a 95 s pull. Prewarm pulls it too
// and says so, and pulls it only when asked: IMAGE... means exactly those images.
func TestPrewarmPullsTheVolumeCopyHelper(t *testing.T) {
	h := &helperPuller{fakePuller{present: map[string]bool{}}}

	var out bytes.Buffer
	if err := Prewarm(context.Background(), h, &out, []string{"redis:7"}, true); err != nil {
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

	if err := Prewarm(context.Background(), h, &out, []string{"redis:7"}, true); err != nil {
		t.Fatal(err)
	}

	if len(h.pulled) != 0 || !strings.Contains(out.String(), "0 pulled, 2 already present") {
		t.Errorf("pulled %v; output:\n%s", h.pulled, out.String())
	}

	// Not asked for helpers: only the named images.
	h.pulled = nil
	h.present = map[string]bool{}

	if err := Prewarm(context.Background(), h, &out, []string{"redis:7"}, false); err != nil {
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
	if err := Prewarm(context.Background(), h, &out, []string{"alpine:3"}, true); err != nil {
		t.Fatal(err)
	}

	if len(h.pulled) != 1 {
		t.Errorf("pulled %v, want alpine:3 once", h.pulled)
	}
}
