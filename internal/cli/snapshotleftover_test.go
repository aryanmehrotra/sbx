package cli

// `kill -9` of `sbx snapshot` mid-copy leaves sbx-snapvol-<name>-<svc> with no image, and
// `sbx snapshot --rm <name>` answered `no snapshot "<name>"` because it looked a snapshot up by
// its images alone - the one thing an interrupted snapshot never got to make.

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// Volumes implements provider.VolumeLister for the fake engine.
func (e *engine) Volumes(_ context.Context, prefix string) ([]string, error) {
	var out []string

	for v := range e.volumes {
		if strings.HasPrefix(v, prefix) {
			out = append(out, v)
		}
	}

	slices.Sort(out)

	return out, nil
}

func TestRemoveSnapshotFindsASnapshotThatIsOnlyVolumes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	e := newEngine()
	e.volumes["sbx-snapvol-gold-db"] = 3    // gold's, left by an interrupted snapshot
	e.volumes["sbx-snapvol-gold-cache"] = 1 // likewise
	// Another snapshot whose name starts with gold-, complete: its volume is claimed by its image.
	e.images["sbx-snap-gold-x-db:latest"] = map[string]string{labelSnapshotName: "gold-x", labelSnapshotVolume: "sbx-snapvol-gold-x-db"}
	e.volumes["sbx-snapvol-gold-x-db"] = 2
	// Unclaimed and dashed: gold's service "y-db", or an interrupted "gold-y"'s "db". Not ours to guess.
	e.volumes["sbx-snapvol-gold-y-db"] = 1

	// Only the unclaimed dashed one is unsure; gold-x's volume is not even mentioned.
	if _, unsure, err := leftoverVolumes(context.Background(), e, e, "gold"); err != nil || !slices.Equal(unsure, []string{"sbx-snapvol-gold-y-db"}) {
		t.Fatalf("unsure = %v, %v; want only sbx-snapvol-gold-y-db", unsure, err)
	}

	if err := RemoveSnapshot(context.Background(), e, "gold"); err != nil {
		t.Fatalf("--rm of a volume-only snapshot: %v", err)
	}

	want := strings.Join([]string{
		"image sbx-snap-gold-x-db:latest", "volume sbx-snapvol-gold-x-db", "volume sbx-snapvol-gold-y-db",
	}, "\n")

	if got := e.state(); got != want {
		t.Fatalf("after --rm gold:\n%s\nwant:\n%s", got, want)
	}

	if err := RemoveSnapshot(context.Background(), e, "gold"); err == nil {
		t.Fatal("removing it again reported success")
	}
}

// Still refused while a container mounts one of those volumes.
func TestRemovingAVolumeOnlySnapshotRefusesWhileMounted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	e := newEngine()
	e.volumes["sbx-snapvol-gold-db"] = 3
	e.users = map[string][]string{"sbx-snapvol-gold-db": {"container debug"}}

	before := e.state()

	err := RemoveSnapshot(context.Background(), e, "gold")
	if err == nil || !strings.Contains(err.Error(), "docker rm debug") {
		t.Fatalf("--rm of a mounted volume-only snapshot = %v, want a refusal naming the container", err)
	}

	if e.state() != before {
		t.Fatalf("a refused --rm removed something:\n%s", e.state())
	}
}

// A partial snapshot - an image for one service, only a volume for another - loses both.
func TestRemoveSnapshotTakesAnImagelessVolumeWithTheRest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	e := newEngine()
	e.images["sbx-snap-gold-db:latest"] = map[string]string{labelSnapshotName: "gold", labelSnapshotVolume: "sbx-snapvol-gold-db"}
	e.volumes["sbx-snapvol-gold-db"] = 3
	e.volumes["sbx-snapvol-gold-cache"] = 1

	if err := RemoveSnapshot(context.Background(), e, "gold"); err != nil {
		t.Fatal(err)
	}

	if got := e.state(); got != "" {
		t.Fatalf("left behind:\n%s", got)
	}
}
