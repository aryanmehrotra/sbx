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

	if !strings.Contains(got, "3 images of snapshots skipped") {
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

	if !strings.Contains(got, "3 images of snapshots skipped") || !strings.Contains(got, "2 newer than 24h0m0s skipped") {
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

	if !strings.Contains(got, "1 image of snapshots skipped") || !strings.Contains(got, "1 newer than 24h0m0s skipped") {
		t.Errorf("nothing reclaimable: want both counts:\n%s", got)
	}
}

// "29 snapshots skipped" counted a snapshot's images and volumes together, so one pg+redis
// snapshot read as four snapshots. The summary says what it counted: images and volumes.
func TestGCSummaryCountsImagesAndVolumesNotSnapshots(t *testing.T) {
	items := []provider.Artifact{
		{Kind: "image", Name: "sbx-snap-a-pg:latest", Snapshot: true, Age: time.Hour},
		{Kind: "image", Name: "sbx-snap-a-redis:latest", Snapshot: true, Age: time.Hour},
		{Kind: "volume", Name: "sbx-snapvol-a-pg", Snapshot: true, Age: time.Hour},
		{Kind: "volume", Name: "sbx-snapvol-a-redis", Snapshot: true, Age: time.Hour},
		{Kind: "volume", Name: "sbx-snapvol-b-db", Snapshot: true, Age: time.Hour},
	}

	var out strings.Builder
	if err := gcWith(context.Background(), &fakeCollector{items: items}, &out, 0, false, false); err != nil {
		t.Fatal(err)
	}

	if got := out.String(); !strings.Contains(got, "2 images and 3 volumes of snapshots skipped") ||
		strings.Contains(got, "5 snapshots") {
		t.Errorf("want the skipped snapshot artifacts counted by kind:\n%s", got)
	}
}
