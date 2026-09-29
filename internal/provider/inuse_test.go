package provider

import (
	"slices"
	"testing"
)

// `sbx gc --snapshots` listed the images a live fork runs from as reclaimable, and --force
// then tried to delete them. A fork's containers are created from its snapshot's images, so
// while the fork exists (asleep or awake) the snapshot is not garbage.
func TestMarkInUseKeepsWhatAForkUses(t *testing.T) {
	arts := []Artifact{
		{Kind: "image", Name: "sbx-snap-base-pg:latest", Snapshot: true},
		{Kind: "image", Name: "sbx-snap-base-redis:latest", Snapshot: true}, // same snapshot, unused itself
		{Kind: "volume", Name: "sbx-snapvol-base-pg", Snapshot: true},
		{Kind: "volume", Name: "sbx-snapvol-base-redis", Snapshot: true},
		{Kind: "image", Name: "sbx-snap-old-pg:latest", Snapshot: true}, // nobody uses it
		{Kind: "volume", Name: "sbx-snapvol-old-pg", Snapshot: true},
		{Kind: "volume", Name: "sbx-gone-db-data"},      // an ordinary orphan
		{Kind: "volume", Name: "sbx-snapvol-legacy-db"}, // mounted by hand
		{Kind: "image", Name: "sbx-snap-vanished-x:latest", Snapshot: true},
	}

	meta := map[string]imageMeta{
		"sbx-snap-base-pg:latest":    {ID: "sha256:aaa", Snapshot: "base", Volume: "sbx-snapvol-base-pg"},
		"sbx-snap-base-redis:latest": {ID: "sha256:bbb", Snapshot: "base", Volume: "sbx-snapvol-base-redis"},
		"sbx-snap-old-pg:latest":     {ID: "sha256:ccc", Snapshot: "old", Volume: "sbx-snapvol-old-pg"},
		// sbx-snap-vanished-x has no metadata: it could not be inspected.
	}

	u := unitUsage{
		images:  map[string][]string{"sha256:aaa": {"agent-1"}},
		volumes: map[string][]string{"sbx-snapvol-legacy-db": {"container scratch"}},
	}

	markInUse(arts, meta, u)

	var inUse, free []string

	for _, a := range arts {
		if a.InUse {
			inUse = append(inUse, a.Name)
		} else {
			free = append(free, a.Name)
		}
	}

	// base whole - an image a fork uses, its sibling image and both volumes, because a sweep
	// that took only the unused half leaves a snapshot a later fork starts half of fresh -
	// the mounted volume, and the image nobody could inspect (unknown is not free).
	want := []string{
		"sbx-snap-base-pg:latest", "sbx-snap-base-redis:latest", "sbx-snap-vanished-x:latest",
		"sbx-snapvol-base-pg", "sbx-snapvol-base-redis", "sbx-snapvol-legacy-db",
	}

	slices.Sort(inUse)

	if !slices.Equal(inUse, want) {
		t.Errorf("in use = %v\nwant     %v", inUse, want)
	}

	slices.Sort(free)

	if wantFree := []string{"sbx-gone-db-data", "sbx-snap-old-pg:latest", "sbx-snapvol-old-pg"}; !slices.Equal(free, wantFree) {
		t.Errorf("free = %v\nwant   %v", free, wantFree)
	}
}

// An unlabelled image (made before snapshots carried labels) pairs with its volume by name.
func TestMarkInUsePairsAnUnlabelledImageWithItsVolumeByName(t *testing.T) {
	arts := []Artifact{
		{Kind: "image", Name: "sbx-snap-gold-db:latest", Snapshot: true},
		{Kind: "volume", Name: "sbx-snapvol-gold-db", Snapshot: true},
	}

	markInUse(arts, map[string]imageMeta{"sbx-snap-gold-db:latest": {ID: "sha256:d"}},
		unitUsage{images: map[string][]string{"sha256:d": {"f"}}})

	if !arts[0].InUse || !arts[1].InUse {
		t.Fatalf("%+v", arts)
	}
}

func TestParseUsage(t *testing.T) {
	// As `{{json .Config}}` prints it: a container sbx did not make may carry no labels at all.
	out := "/sbx-agent-1-pg\tsha256:aaa\tsbx-agent-1-pg-data \t{\"Labels\":{\"sbx.sandbox\":\"agent-1\"}}\n" +
		"/scratch\tsha256:bbb\tsbx-snapvol-x-db other \t{\"Cmd\":[\"sh\"]}\n" +
		"/sbx-agent-2-pg\tsha256:aaa\t\t{\"Labels\":{\"sbx.sandbox\":\"agent-2\"}}\n"

	u := parseUsage(out)

	if got := u.images["sha256:aaa"]; !slices.Equal(got, []string{"agent-1", "agent-2"}) {
		t.Errorf("users of aaa = %v", got)
	}

	if got := u.volumes["sbx-snapvol-x-db"]; !slices.Equal(got, []string{"container scratch"}) {
		t.Errorf("users of the snapvol = %v", got)
	}

	if got := u.images["sha256:bbb"]; !slices.Equal(got, []string{"container scratch"}) {
		t.Errorf("users of bbb = %v", got)
	}
}

// A snapshot volume no listed image claims is what `kill -9` of `sbx snapshot` mid-copy leaves.
// gc names it, so the leftover reads as one instead of as half of some snapshot.
func TestMarkNoImage(t *testing.T) {
	arts := []Artifact{
		{Kind: "image", Name: "sbx-snap-gold-db:latest", Snapshot: true},
		{Kind: "image", Name: "sbx-snap-old-web:latest", Snapshot: true}, // unlabelled: pairs by name
		{Kind: "volume", Name: "sbx-snapvol-gold-data", Snapshot: true},  // gold-db's, by label
		{Kind: "volume", Name: "sbx-snapvol-old-web", Snapshot: true},
		{Kind: "volume", Name: "sbx-snapvol-cut-db", Snapshot: true}, // no image at all
		{Kind: "volume", Name: "sbx-gone-db-data"},                   // not a snapshot's
	}

	markNoImage(arts, map[string]imageMeta{
		"sbx-snap-gold-db:latest": {ID: "a", Snapshot: "gold", Volume: "sbx-snapvol-gold-data"},
		"sbx-snap-old-web:latest": {ID: "b"},
	})

	for _, a := range arts {
		if want := a.Name == "sbx-snapvol-cut-db"; a.NoImage != want {
			t.Errorf("%s: NoImage = %v, want %v", a.Name, a.NoImage, want)
		}
	}
}

// On a machine where other sandboxes come and go, a container removed between `docker ps` and
// `docker inspect` fails the inspect (exit 1) though every other container was read. That one
// uses nothing any more, so its absence is not a reason to refuse: gc and --rm failed with
// "could not tell whether a sandbox still uses" while other agents churned containers. Any
// other failure still is.
func TestOnlyVanished(t *testing.T) {
	for stderr, want := range map[string]bool{
		"Error: No such container: 5c7511dc3ea6\n":                   true,
		"Error: No such container: a\nError: No such container: b\n": true,
		"Error response from daemon: No such container: a\n":         true,
		"":                                    false, // failed with no reason given
		"Cannot connect to the Docker daemon": false,
		"Error: No such container: a\ntemplate parsing error: map has no entry": false,
	} {
		if got := onlyVanished(stderr); got != want {
			t.Errorf("onlyVanished(%q) = %v, want %v", stderr, got, want)
		}
	}
}
