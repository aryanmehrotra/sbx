package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// `sbx gc --snapshots` offered the snapshot a live fork runs from, and --force tried to delete
// it. An artifact in use is never offered, --force or not, and the output says how many.
func TestGCNeverOffersWhatAForkStillUses(t *testing.T) {
	f := &fakeCollector{items: []provider.Artifact{
		{Kind: "image", Name: "sbx-snap-base-pg:latest", Snapshot: true, InUse: true, Age: 72 * time.Hour},
		{Kind: "volume", Name: "sbx-snapvol-base-pg", Snapshot: true, InUse: true, Age: 72 * time.Hour},
		{Kind: "image", Name: "sbx-snap-old-pg:latest", Snapshot: true, Age: 72 * time.Hour},
	}}

	for _, force := range []bool{false, true} {
		f.reclaimed = nil

		var out strings.Builder
		if err := gcWith(context.Background(), f, &out, 0, force, true); err != nil {
			t.Fatalf("GC force=%v: %v", force, err)
		}

		if strings.Contains(out.String(), "sbx-snap-base-pg") || strings.Contains(out.String(), "sbx-snapvol-base-pg") {
			t.Errorf("force=%v: an artifact a fork uses was offered:\n%s", force, out.String())
		}

		for _, r := range f.reclaimed {
			if strings.Contains(r, "base") {
				t.Errorf("force=%v: reclaimed %s, which a fork still uses", force, r)
			}
		}

		if !strings.Contains(out.String(), "1 image and 1 volume in use") {
			t.Errorf("force=%v: the output does not say 2 were skipped for being in use:\n%s", force, out.String())
		}
	}

	// And with nothing else to reclaim, "nothing to reclaim" still says why.
	f.items = f.items[:2]
	f.reclaimed = nil

	var out strings.Builder
	if err := gcWith(context.Background(), f, &out, 0, true, true); err != nil {
		t.Fatal(err)
	}

	if len(f.reclaimed) != 0 || !strings.Contains(out.String(), "1 image and 1 volume in use") {
		t.Errorf("reclaimed %v; output:\n%s", f.reclaimed, out.String())
	}
}

// A snapshot volume with no image is named as an interrupted snapshot's, with the command that
// removes it.
func TestGCNamesAVolumeOnlySnapshot(t *testing.T) {
	f := &fakeCollector{items: []provider.Artifact{
		{Kind: "volume", Name: "sbx-snapvol-cut-db", Snapshot: true, NoImage: true, SnapshotName: "cut", Age: time.Hour},
	}}

	var out strings.Builder
	if err := gcWith(context.Background(), f, &out, 0, false, true); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "no image") || !strings.Contains(out.String(), "sbx snapshot --rm cut") {
		t.Errorf("the leftover is not named as one:\n%s", out.String())
	}
}
