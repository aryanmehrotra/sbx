package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A fake collector, so the rules can be tested without a docker daemon. What matters here
// is which artifacts are OFFERED and whether anything is deleted - deleting data is the
// one operation that cannot be taken back.
type fakeCollector struct {
	items     []provider.Artifact
	reclaimed []string
}

func (f *fakeCollector) Orphans(context.Context) ([]provider.Artifact, error) { return f.items, nil }

func (f *fakeCollector) Reclaim(_ context.Context, a provider.Artifact) error {
	f.reclaimed = append(f.reclaimed, a.Name)
	return nil
}

// The rest of Provider, unused: GC only ever touches the Collector half.
func (f *fakeCollector) Name() string { return "fake" }

func TestGCListsAndDeletesNothingByDefault(t *testing.T) {
	f := &fakeCollector{items: []provider.Artifact{
		{Kind: "volume", Name: "sbx-gone-pg-data", Age: 72 * time.Hour},
	}}

	var out strings.Builder
	if err := gcWith(context.Background(), f, &out, 0, false, false); err != nil {
		t.Fatalf("GC: %v", err)
	}

	if len(f.reclaimed) != 0 {
		t.Errorf("GC deleted something without --force: %v", f.reclaimed)
	}

	if !strings.Contains(out.String(), "sbx-gone-pg-data") {
		t.Errorf("the artifact was not listed:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "--force") {
		t.Errorf("the output does not say how to actually delete:\n%s", out.String())
	}
}

func TestGCForceReclaims(t *testing.T) {
	f := &fakeCollector{items: []provider.Artifact{
		{Kind: "volume", Name: "sbx-gone-pg-data", Age: 72 * time.Hour},
	}}

	var out strings.Builder
	if err := gcWith(context.Background(), f, &out, 0, true, false); err != nil {
		t.Fatalf("GC: %v", err)
	}

	if len(f.reclaimed) != 1 || f.reclaimed[0] != "sbx-gone-pg-data" {
		t.Errorf("reclaimed %v, want the one orphan", f.reclaimed)
	}
}

// A snapshot was made deliberately and by name, and outliving its sandbox is the entire
// point of one. Sweeping it with the volumes would delete something someone chose to keep.
func TestGCLeavesSnapshotsAloneUnlessAsked(t *testing.T) {
	items := []provider.Artifact{
		{Kind: "volume", Name: "sbx-gone-pg-data", Age: 72 * time.Hour},
		{Kind: "image", Name: "sbx-snap-golden-pg:latest", Age: 72 * time.Hour, Snapshot: true},
	}

	f := &fakeCollector{items: items}

	var out strings.Builder
	_ = gcWith(context.Background(), f, &out, 0, true, false)

	if len(f.reclaimed) != 1 || strings.Contains(strings.Join(f.reclaimed, ","), "snap") {
		t.Errorf("a snapshot was swept without --snapshots: %v", f.reclaimed)
	}

	f2 := &fakeCollector{items: items}

	var out2 strings.Builder
	_ = gcWith(context.Background(), f2, &out2, 0, true, true)

	if len(f2.reclaimed) != 2 {
		t.Errorf("--snapshots did not include the snapshot: %v", f2.reclaimed)
	}
}

// Age is the other guard. A brand-new orphan under a threshold must be skipped, and the
// output has to say why rather than reporting nothing at all.
func TestGCHonoursAge(t *testing.T) {
	f := &fakeCollector{items: []provider.Artifact{
		{Kind: "volume", Name: "sbx-fresh-pg-data", Age: time.Minute},
	}}

	var out strings.Builder
	_ = gcWith(context.Background(), f, &out, 24*time.Hour, true, false)

	if len(f.reclaimed) != 0 {
		t.Errorf("an artifact younger than the threshold was deleted: %v", f.reclaimed)
	}

	if !strings.Contains(out.String(), "skipped") {
		t.Errorf("the output does not explain the skip:\n%s", out.String())
	}
}
