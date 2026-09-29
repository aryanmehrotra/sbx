package cli

// A snapshot name is taken once. `sbx snapshot <sandbox> <existing>` used to commit over the old
// images and add its own beside them, so a snapshot of pg+redis re-taken from a sandbox with one
// service r became mix-r, mix-pg and mix-redis - and every fork of it failed on `the spec has no
// service "r"`. Re-taking it from the same sandbox silently replaced the image under every fork
// that still meant the old one. These tests compare what is left in the engine.

import (
	"context"
	"strings"
	"testing"
)

// users is the engine's answer to "which sandbox still uses this image or volume".
func (e *engine) InUse(_ context.Context, images, volumes []string) (map[string][]string, error) {
	out := map[string][]string{}

	for _, n := range append(append([]string{}, images...), volumes...) {
		if u := e.users[n]; len(u) > 0 {
			out[n] = u
		}
	}

	return out, nil
}

func mixEngine() *engine {
	e := newEngine(units("a", "pg", "redis")...)
	e.volumes["sbx-a-pg-data"] = 3
	e.volumes["sbx-a-redis-data"] = 2

	return e
}

func TestSnapshotRefusesANameThatExists(t *testing.T) {
	e := mixEngine()

	if _, err := Snapshot(context.Background(), e, "a", "mix"); err != nil {
		t.Fatal(err)
	}

	// Another sandbox with a different service, under the same name: the merge.
	e.units = units("b", "r")
	e.volumes["sbx-b-r-data"] = 1
	before := e.state()

	_, err := Snapshot(context.Background(), e, "b", "mix")
	if err == nil {
		t.Fatal("snapshotting under an existing name succeeded")
	}

	for _, want := range []string{`"mix"`, "already exists", "sbx snapshot --rm mix", "--replace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}

	if after := e.state(); after != before {
		t.Fatalf("a refused snapshot changed the engine:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// And the same sandbox again: its images would be replaced under every fork of them.
	e.units = units("a", "pg", "redis")

	if _, err := Snapshot(context.Background(), e, "a", "mix"); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("re-snapshotting the same sandbox under its name = %v, want a refusal", err)
	}
}

// A volume left under the name with no image (an interrupted sweep) is still the name taken:
// the copy would have landed on it.
func TestSnapshotRefusesALeftoverSnapshotVolume(t *testing.T) {
	e := mixEngine()
	e.volumes["sbx-snapvol-gold-pg"] = 5

	before := e.state()

	if _, err := Snapshot(context.Background(), e, "a", "gold"); err == nil ||
		!strings.Contains(err.Error(), "sbx-snapvol-gold-pg") {
		t.Fatalf("a snapshot over a leftover volume = %v, want a refusal naming it", err)
	}

	if after := e.state(); after != before {
		t.Fatalf("a refused snapshot changed the engine:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// "qa" of a service "s1-db" writes sbx-snap-qa-s1-db, which is also snapshot "qa-s1"'s image of
// its service "db". The label says whose it is, and it is not ours to overwrite.
func TestSnapshotRefusesAnotherSnapshotsImageAtItsName(t *testing.T) {
	e := newEngine(units("a", "s1-db")...)
	e.images["sbx-snap-qa-s1-db:latest"] = map[string]string{labelSnapshotName: "qa-s1", labelSnapshotVolume: noVolume}

	_, err := Snapshot(context.Background(), e, "a", "qa")
	if err == nil || !strings.Contains(err.Error(), `"qa-s1"`) {
		t.Fatalf("overwriting another snapshot's image = %v, want a refusal naming qa-s1", err)
	}

	if e.images["sbx-snap-qa-s1-db:latest"][labelSnapshotName] != "qa-s1" {
		t.Fatal("qa-s1's image was overwritten")
	}

	// --replace does not make it ours either.
	if _, err := ReplaceSnapshot(context.Background(), e, "a", "qa"); err == nil {
		t.Fatal("--replace overwrote another snapshot's image")
	}

	if e.images["sbx-snap-qa-s1-db:latest"][labelSnapshotName] != "qa-s1" {
		t.Fatal("qa-s1's image was overwritten by --replace")
	}
}

// --replace removes the old snapshot entirely and takes a fresh one. It is never a merge.
func TestReplaceSnapshotIsNotAMerge(t *testing.T) {
	e := mixEngine()

	if _, err := Snapshot(context.Background(), e, "a", "mix"); err != nil {
		t.Fatal(err)
	}

	e.units = units("b", "r")
	e.volumes["sbx-b-r-data"] = 1

	if _, err := ReplaceSnapshot(context.Background(), e, "b", "mix"); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	refs, err := SnapshotsOf(context.Background(), e, "mix")
	if err != nil {
		t.Fatal(err)
	}

	if len(refs) != 1 || refs[0].Service != "r" {
		t.Fatalf("after --replace the snapshot is %+v, want only service r", refs)
	}

	for _, gone := range []string{"sbx-snapvol-mix-pg", "sbx-snapvol-mix-redis"} {
		if _, ok := e.volumes[gone]; ok {
			t.Errorf("the old snapshot's %s survived --replace", gone)
		}
	}

	if e.volumes["sbx-snapvol-mix-r"] != 1 {
		t.Errorf("the fresh snapshot did not carry r's data: %v", e.volumes)
	}
}

// --replace of a name nobody has taken is a plain snapshot.
func TestReplaceSnapshotOfANewName(t *testing.T) {
	e := mixEngine()

	if _, err := ReplaceSnapshot(context.Background(), e, "a", "fresh"); err != nil {
		t.Fatalf("ReplaceSnapshot of a new name: %v", err)
	}

	if _, err := SnapshotsOf(context.Background(), e, "fresh"); err != nil {
		t.Fatal(err)
	}
}

// A fork still created from the snapshot keeps it: --replace and --rm refuse before removing
// anything, rather than removing what docker lets go and failing on the image it does not.
func TestReplaceAndRemoveRefuseWhileAForkUsesTheSnapshot(t *testing.T) {
	e := mixEngine()

	if _, err := Snapshot(context.Background(), e, "a", "mix"); err != nil {
		t.Fatal(err)
	}

	e.users = map[string][]string{"sbx-snap-mix-redis:latest": {"agent-1"}}
	before := e.state()

	for name, run := range map[string]func() error{
		"--replace": func() error { _, err := ReplaceSnapshot(context.Background(), e, "a", "mix"); return err },
		"--rm":      func() error { return RemoveSnapshot(context.Background(), e, "mix") },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "agent-1") || !strings.Contains(err.Error(), "sbx rm agent-1") {
			t.Errorf("%s while a fork uses the snapshot = %v, want a refusal naming agent-1", name, err)
		}

		if after := e.state(); after != before {
			t.Fatalf("%s removed part of a snapshot a fork still uses:\nbefore:\n%s\nafter:\n%s", name, before, after)
		}
	}
}

// --replace of a sandbox that does not exist must not delete the snapshot first and then fail.
func TestReplaceOfAnUnknownSandboxKeepsTheOldSnapshot(t *testing.T) {
	e := mixEngine()

	if _, err := Snapshot(context.Background(), e, "a", "mix"); err != nil {
		t.Fatal(err)
	}

	before := e.state()
	e.units = nil

	if _, err := ReplaceSnapshot(context.Background(), e, "gone", "mix"); err == nil {
		t.Fatal("--replace of a sandbox that does not exist succeeded")
	}

	if after := e.state(); after != before {
		t.Fatalf("--replace of an unknown sandbox changed the engine:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
