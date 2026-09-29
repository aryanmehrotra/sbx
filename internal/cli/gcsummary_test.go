package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Without --older-than nothing can be "newer than 0s", yet the summary said "24 more were skipped
// for being newer than 0s, or for being snapshots" - one lumped number, half of whose reason was
// impossible. Each reason gets its own count, and a reason that did not apply is not mentioned.
func TestGCSummaryCountsEachSkipReasonSeparately(t *testing.T) {
	var items []provider.Artifact

	for range 3 {
		items = append(items, provider.Artifact{Kind: "image", Name: "snap", Age: 72 * time.Hour, Snapshot: true})
	}

	items = append(items, provider.Artifact{Kind: "volume", Name: "sbx-gone-pg-data", Age: 72 * time.Hour})

	var out strings.Builder
	if err := gcWith(context.Background(), &fakeCollector{items: items}, &out, 0, false, false); err != nil {
		t.Fatal(err)
	}

	got := out.String()

	if strings.Contains(got, "newer than") {
		t.Errorf("no --older-than, so nothing was skipped for age, but the summary says so:\n%s", got)
	}

	if !strings.Contains(got, "3 snapshots skipped") {
		t.Errorf("the snapshot count is missing or wrong:\n%s", got)
	}

	// With an age cut-off, the two reasons are counted apart.
	items = append(items,
		provider.Artifact{Kind: "volume", Name: "fresh-1", Age: time.Hour},
		provider.Artifact{Kind: "volume", Name: "fresh-2", Age: time.Hour})

	out.Reset()

	if err := gcWith(context.Background(), &fakeCollector{items: items}, &out, 24*time.Hour, false, false); err != nil {
		t.Fatal(err)
	}

	got = out.String()

	if !strings.Contains(got, "3 snapshots skipped") || !strings.Contains(got, "2 newer than 24h0m0s skipped") {
		t.Errorf("want separate counts for snapshots (3) and age (2):\n%s", got)
	}

	// And when nothing at all is reclaimable, the same two counts.
	out.Reset()

	young := []provider.Artifact{
		{Kind: "volume", Name: "fresh-1", Age: time.Hour},
		{Kind: "image", Name: "snap", Age: 72 * time.Hour, Snapshot: true},
	}

	if err := gcWith(context.Background(), &fakeCollector{items: young}, &out, 24*time.Hour, false, false); err != nil {
		t.Fatal(err)
	}

	got = out.String()

	if !strings.Contains(got, "1 snapshot skipped") || !strings.Contains(got, "1 newer than 24h0m0s skipped") {
		t.Errorf("nothing reclaimable: want both counts:\n%s", got)
	}
}
