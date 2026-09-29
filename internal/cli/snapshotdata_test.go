package cli

// Snapshot, fork and snapshot removal against a fake engine that keeps state the way docker
// does: a mount creates a missing volume, a commit writes an image with labels. The bugs these
// guard were all "reported one thing, left another", so every test compares what is left in
// the engine, not what the function returned.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

type engine struct {
	provider.Provider

	units     []provider.Unit
	images    map[string]map[string]string // image -> labels
	volumes   map[string]int               // volume -> entries in it
	copyErr   map[string]error             // src -> error CopyVolume returns
	commitErr map[string]error             // image -> error Commit returns
	users     map[string][]string          // image or volume -> sandboxes using it (InUse)
	events    []string                     // pause, copy, commit, unpause, in the order they happened
	volLabels map[string]map[string]string // volume -> labels it was created with
}

func newEngine(units ...provider.Unit) *engine {
	return &engine{units: units, images: map[string]map[string]string{}, volumes: map[string]int{}, copyErr: map[string]error{}, commitErr: map[string]error{}}
}

func (e *engine) Name() string                                          { return "docker" }
func (e *engine) List(context.Context, string) ([]provider.Unit, error) { return e.units, nil }
func (e *engine) VolumeFor(sandbox, service string) string {
	return "sbx-" + sandbox + "-" + service + "-data"
}

func (e *engine) Commit(_ context.Context, ref, image string, changes ...string) error {
	e.events = append(e.events, "commit "+ref)

	if err := e.commitErr[image]; err != nil {
		return err
	}

	labels := map[string]string{}

	for _, c := range changes {
		if kv, ok := strings.CutPrefix(c, "LABEL "); ok {
			k, v, _ := strings.Cut(kv, "=")
			labels[k] = v
		}
	}

	e.images[image] = labels

	return nil
}

// CopyVolume behaves as a mount does: asked to copy from a volume that is not there, it
// creates it, so the CLI must not ask. An empty source is refused with ErrEmptyVolume.
func (e *engine) CopyVolume(_ context.Context, src, dst string) error {
	e.events = append(e.events, "copy "+src)

	if _, ok := e.volumes[src]; !ok {
		e.volumes[src] = 0
	}

	if err := e.copyErr[src]; err != nil {
		if _, ok := e.volumes[dst]; !ok {
			e.volumes[dst] = 0 // the mount made it before the copy failed
		}

		return err
	}

	if e.volumes[src] == 0 {
		return fmt.Errorf("copying %s: %w", src, provider.ErrEmptyVolume)
	}

	e.volumes[dst] = e.volumes[src]

	return nil
}

func (e *engine) Images(_ context.Context, prefix string) ([]string, error) {
	var out []string

	for img := range e.images {
		if strings.HasPrefix(img, prefix) {
			out = append(out, img)
		}
	}

	slices.Sort(out)

	return out, nil
}

func (e *engine) RemoveImage(_ context.Context, image string) error {
	delete(e.images, image)
	return nil
}

func (e *engine) ImageLabel(_ context.Context, image, key string) (string, error) {
	return e.images[image][key], nil
}

func (e *engine) VolumeExists(_ context.Context, name string) (bool, error) {
	_, ok := e.volumes[name]
	return ok, nil
}

func (e *engine) CreateVolume(_ context.Context, name string, labels map[string]string) error {
	e.volumes[name] = 0

	if e.volLabels == nil {
		e.volLabels = map[string]map[string]string{}
	}

	e.volLabels[name] = labels
	return nil
}

func (e *engine) RemoveVolume(_ context.Context, name string) error {
	delete(e.volumes, name)
	delete(e.volLabels, name)
	return nil
}

func (e *engine) state() string {
	var parts []string
	for i := range e.images {
		parts = append(parts, "image "+i)
	}

	for v := range e.volumes {
		parts = append(parts, "volume "+v)
	}

	slices.Sort(parts)

	return strings.Join(parts, "\n")
}

func units(sandbox string, services ...string) []provider.Unit {
	var us []provider.Unit
	for _, s := range services {
		us = append(us, provider.Unit{Sandbox: sandbox, Service: s, Ref: "sbx-" + sandbox + "-" + s, Running: true})
	}

	return us
}

// A service that declared no `volume` has nothing on a volume to carry. It used to be copied
// from anyway: the mount created an empty volume, the copy refused it, and the whole snapshot
// failed after committing an image - leaving that image, an empty snapshot volume and a stray
// source volume behind a message that said nothing was changed.
func TestSnapshotOfAServiceWithoutAVolumeIsImageOnly(t *testing.T) {
	e := newEngine(units("app", "db", "web")...)
	e.volumes["sbx-app-db-data"] = 3 // db declared a volume; web did not

	refs, err := Snapshot(context.Background(), e, "app", "gold")
	if err != nil {
		t.Fatalf("snapshotting a sandbox with a volume-less service failed: %v", err)
	}

	if _, ok := e.volumes["sbx-app-web-data"]; ok {
		t.Error("a stray source volume was created for the service that has none")
	}

	if _, ok := e.volumes["sbx-snapvol-gold-web"]; ok {
		t.Error("an empty snapshot volume was created for the service that has none")
	}

	if e.volumes["sbx-snapvol-gold-db"] != 3 {
		t.Errorf("db's data was not carried: %v", e.volumes)
	}

	for _, r := range refs {
		if r.Service == "web" && r.Volume != "" {
			t.Errorf("web's ref names a volume it does not have: %+v", r)
		}
	}

	// And a fork of it resolves web as image-only, rather than copying from nothing.
	got, err := SnapshotsOf(context.Background(), e, "gold")
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range got {
		if r.Service == "web" && r.Volume != "" {
			t.Errorf("SnapshotsOf gives web a volume the snapshot never made: %+v", r)
		}

		if r.Service == "db" && r.Volume != "sbx-snapvol-gold-db" {
			t.Errorf("SnapshotsOf lost db's volume: %+v", r)
		}
	}
}

// When a snapshot genuinely cannot be taken, everything this call made is removed, so "nothing
// was saved" is true and no half-made snapshot is left for a fork to find.
func TestAFailedSnapshotLeavesNothingBehind(t *testing.T) {
	e := newEngine(units("app", "db", "cache")...)
	e.volumes["sbx-app-db-data"] = 3
	e.volumes["sbx-app-cache-data"] = 1
	e.copyErr["sbx-app-cache-data"] = errors.New("no space left on device")

	before := e.state()

	_, err := Snapshot(context.Background(), e, "app", "gold")
	if err == nil {
		t.Fatal("a snapshot whose copy failed reported success")
	}

	if after := e.state(); after != before {
		t.Fatalf("a failed snapshot left debris:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	if _, err := SnapshotsOf(context.Background(), e, "gold"); err == nil {
		t.Fatal("a fork could still find the snapshot that failed")
	}
}

// Every fork of one snapshot wrote the same sandbox.<snapshot>.json, so the second fork
// overwrote the first one's spec and `sbx env first --spec ...` read the other's.
func TestForkNamesItsSpecAfterTheFork(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "sandbox.json")

	if err := os.WriteFile(specPath, []byte(`{"version":1,"services":{"db":{"image":"redis:7-alpine","ports":[6379],"volume":"/data"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	e := newEngine()
	e.images["sbx-snap-gold-db:latest"] = map[string]string{}
	e.copyErr["sbx-snapvol-gold-db"] = errors.New("stop here, before Create")

	_ = Fork(context.Background(), e, specPath, "gold", "agent-1", false, provider.IsolationContainer)

	if _, err := os.Stat(filepath.Join(dir, "sandbox.agent-1.json")); err != nil {
		t.Fatalf("the fork's spec is not named after the fork: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "sandbox.gold.json")); err == nil {
		t.Fatal("the fork's spec is named after the snapshot, which every fork of it shares")
	}
}

// `sbx snapshot rm` removes one snapshot's images and volumes - and only that one's, although
// its name is a prefix of another's.
func TestRemoveSnapshotRemovesOnlyThatSnapshot(t *testing.T) {
	e := newEngine()
	e.images["sbx-snap-qa-redis:latest"] = map[string]string{labelSnapshotName: "qa"}
	e.volumes["sbx-snapvol-qa-redis"] = 2
	e.images["sbx-snap-qa-s1-pg:latest"] = map[string]string{}                          // older, unlabelled, of "qa-s1"
	e.images["sbx-snap-qa-s3-r:latest"] = map[string]string{labelSnapshotName: "qa-s3"} // labelled, of "qa-s3"
	e.volumes["sbx-snapvol-qa-s1-pg"] = 1
	e.volumes["sbx-snapvol-qa-s3-r"] = 1

	if err := RemoveSnapshot(context.Background(), e, "qa"); err != nil {
		t.Fatalf("RemoveSnapshot: %v", err)
	}

	want := strings.Join([]string{
		"image sbx-snap-qa-s1-pg:latest", "image sbx-snap-qa-s3-r:latest",
		"volume sbx-snapvol-qa-s1-pg", "volume sbx-snapvol-qa-s3-r",
	}, "\n")

	if got := e.state(); got != want {
		t.Fatalf("after rm qa:\n%s\nwant:\n%s", got, want)
	}

	if err := RemoveSnapshot(context.Background(), e, "qa-s1"); err != nil {
		t.Fatalf("RemoveSnapshot qa-s1: %v", err)
	}

	if strings.Contains(e.state(), "qa-s1") {
		t.Fatalf("an unlabelled snapshot was not removed by its own name:\n%s", e.state())
	}

	if err := RemoveSnapshot(context.Background(), e, "nope"); err == nil {
		t.Fatal("removing a snapshot that does not exist reported success")
	}
}

// A declared volume with nothing in it yet is the state a fresh volume starts in, so the
// service is saved as its image, not failed.
func TestSnapshotOfAnEmptyVolumeIsImageOnly(t *testing.T) {
	e := newEngine(units("app", "db", "cache")...)
	e.volumes["sbx-app-db-data"] = 3
	e.volumes["sbx-app-cache-data"] = 0

	refs, err := Snapshot(context.Background(), e, "app", "gold")
	if err != nil {
		t.Fatalf("an empty declared volume failed the snapshot: %v", err)
	}

	for _, r := range refs {
		if r.Service == "cache" && r.Volume != "" {
			t.Errorf("cache's ref names a volume that holds nothing: %+v", r)
		}
	}

	if got := e.images["sbx-snap-gold-cache:latest"][labelSnapshotVolume]; got != noVolume {
		t.Errorf("cache's image is labelled volume=%q, want %q", got, noVolume)
	}
}

// A commit that fails after others succeeded takes back the images and volumes already made:
// SnapshotsOf finds a snapshot by its images, so one left behind is a fork of half a sandbox.
func TestAFailedCommitRemovesTheImagesAlreadyMade(t *testing.T) {
	e := newEngine(units("app", "db", "web")...)
	e.volumes["sbx-app-db-data"] = 3
	e.commitErr["sbx-snap-gold-web:latest"] = errors.New("no space left on device")

	before := e.state()

	if _, err := Snapshot(context.Background(), e, "app", "gold"); err == nil {
		t.Fatal("a snapshot whose commit failed reported success")
	}

	if after := e.state(); after != before {
		t.Fatalf("a failed commit left debris:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A `build` service forked with its image replaced still carried `build`, and a spec with both
// is refused - so every fork of a sandbox with a built service failed after restoring its data.
func TestForkSpecDropsBuildForTheSnapshotImage(t *testing.T) {
	sp := map[string]any{"services": map[string]any{
		"app": map[string]any{"build": map[string]any{"context": "./app"}, "ports": []any{8080.0}},
	}}

	if err := ForkSpec(sp, "gold", []SnapshotRef{{Service: "app", Image: "sbx-snap-gold-app:latest"}}); err != nil {
		t.Fatal(err)
	}

	app := sp["services"].(map[string]any)["app"].(map[string]any)
	if _, ok := app["build"]; ok {
		t.Fatalf("the forked service keeps build beside its snapshot image: %v", app)
	}

	if app["image"] != "sbx-snap-gold-app:latest" {
		t.Fatalf("image = %v", app["image"])
	}
}
