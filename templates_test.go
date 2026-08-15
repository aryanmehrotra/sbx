package main

import (
	"strings"
	"testing"
)

// A floating tag in a template means the first thing anybody runs can change without a commit
// touching this repo — and the failure surfaces as "sbx is broken", not "the upstream image
// moved". CI runs the same check as a shell script; this one keeps it true for `go test`
// alone, which is what somebody editing a template actually runs.
func TestEveryTemplateImageIsPinned(t *testing.T) {
	images := TemplateImages()
	if len(images) == 0 {
		t.Fatal("no template images found — the parser is broken, which would make this test vacuous")
	}

	for _, img := range images {
		if !strings.Contains(img, "@sha256:") {
			t.Errorf("%s is not pinned by digest", img)
		}
	}
}

// The digest is what docker resolves; the tag beside it is documentation. A pin with no tag
// tells a reader nothing about what they are about to run.
func TestPinnedImagesKeepTheirTag(t *testing.T) {
	for _, img := range TemplateImages() {
		name, _, ok := strings.Cut(img, "@")
		if !ok {
			continue // already reported by the pin test
		}

		if !strings.Contains(name, ":") {
			t.Errorf("%s is pinned but carries no tag — nobody can tell what it is", img)
		}
	}
}

// The date is the whole reason pinning is honest rather than a slow-motion staleness bug: it
// is what somebody reads to decide the pins need refreshing.
func TestTemplatesReportWhenTheyWerePinned(t *testing.T) {
	d := TemplatesRefreshed()
	if d == "" {
		t.Fatal("no refresh date — a pin nobody can see the age of is one nobody refreshes")
	}

	if len(d) != len("2006-01-02") || strings.Count(d, "-") != 2 {
		t.Errorf("refresh date %q is not YYYY-MM-DD", d)
	}
}

// Every template must be listed and materializable, or `--template x` is a promise the
// binary does not keep.
func TestEveryTemplateMaterializes(t *testing.T) {
	names := TemplateNames()
	if len(names) == 0 {
		t.Fatal("no templates")
	}

	for _, n := range names {
		if _, err := MaterializeTemplate(n); err != nil {
			t.Errorf("template %s does not materialize: %v", n, err)
		}
	}

	if _, err := MaterializeTemplate("no-such-template"); err == nil {
		t.Error("an unknown template was accepted")
	}
}
