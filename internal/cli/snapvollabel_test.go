package cli

// gc's "no image" hint named the snapshot by reading the volume name up to its last dash, which
// is the wrong snapshot whenever the service has a dash: sbx-snapvol-a-web-ui is snapshot "a"'s
// service "web-ui", and the hint said `sbx snapshot --rm a-web`. A snapshot volume now carries
// its snapshot's name as a label, and where it has none the hint gives the one command that is
// never wrong - `docker volume rm` of that volume.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// VolumeLabel implements provider.VolumeLabeler for the fake engine.
func (e *engine) VolumeLabel(_ context.Context, volume, key string) (string, error) {
	return e.volLabels[volume][key], nil
}

func TestASnapshotVolumeIsLabelledWithItsSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	e := newEngine(units("app", "web-ui", "cache")...)
	e.volumes["sbx-app-web-ui-data"] = 3
	e.volumes["sbx-app-cache-data"] = 0 // empty: image-only, and no volume may be left for it

	if _, err := Snapshot(context.Background(), e, "app", "gold"); err != nil {
		t.Fatal(err)
	}

	if got := e.volLabels["sbx-snapvol-gold-web-ui"][labelSnapshotName]; got != "gold" {
		t.Errorf("the snapshot volume is labelled %q, want gold", got)
	}

	if _, ok := e.volumes["sbx-snapvol-gold-cache"]; ok {
		t.Error("an empty service left a labelled snapshot volume behind")
	}
}

// A labelled volume with a dash in its service part is resolvable, so --rm takes it; one
// labelled as another snapshot's is not this one's, and is not even "unsure".
func TestRemoveSnapshotTrustsTheVolumeLabel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	e := newEngine()
	e.volumes["sbx-snapvol-a-web-ui"] = 1
	e.volumes["sbx-snapvol-a-b-db"] = 1
	e.volLabels = map[string]map[string]string{
		"sbx-snapvol-a-web-ui": {labelSnapshotName: "a"},
		"sbx-snapvol-a-b-db":   {labelSnapshotName: "a-b"},
	}

	ours, unsure, err := leftoverVolumes(context.Background(), e, e, "a")
	if err != nil || len(unsure) != 0 || len(ours) != 1 || ours[0] != "sbx-snapvol-a-web-ui" {
		t.Fatalf("ours %v unsure %v (%v); want only a-web-ui as ours", ours, unsure, err)
	}
}

func TestGCHintNeverGuessesTheSnapshotName(t *testing.T) {
	f := &fakeCollector{items: []provider.Artifact{
		{Kind: "volume", Name: "sbx-snapvol-a-web-ui", Snapshot: true, NoImage: true, SnapshotName: "a", Age: time.Hour},
		{Kind: "volume", Name: "sbx-snapvol-old-web-ui", Snapshot: true, NoImage: true, Age: time.Hour},
	}}

	var out strings.Builder
	if err := gcWith(context.Background(), f, &out, 0, false, true); err != nil {
		t.Fatal(err)
	}

	got := out.String()

	if !strings.Contains(got, "sbx-snapvol-a-web-ui has no image") || !strings.Contains(got, "sbx snapshot --rm a removes it") {
		t.Errorf("a labelled leftover is not resolved by its label:\n%s", got)
	}

	if strings.Contains(got, "--rm a-web") || strings.Contains(got, "--rm old-web") {
		t.Errorf("the hint guessed a snapshot name from the volume name:\n%s", got)
	}

	if !strings.Contains(got, "docker volume rm sbx-snapvol-old-web-ui") {
		t.Errorf("an unlabelled leftover does not get the docker volume rm command:\n%s", got)
	}
}
