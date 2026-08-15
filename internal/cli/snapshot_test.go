package cli

import "testing"

// The rules ForkSpec enforces are the ones that were got wrong first, so each has a test
// naming the failure rather than the rule.
func TestForkSpecRewritesImageAndKeepsVolume(t *testing.T) {
	sp := map[string]any{
		"services": map[string]any{
			"postgres": map[string]any{
				"image":  "postgres:16-alpine",
				"volume": "/var/lib/postgresql/data",
				"init":   []any{"psql -c 'create table t(v text)'"},
				"ports":  []any{5432.0},
			},
		},
	}

	refs := []SnapshotRef{{Service: "postgres", Image: "snap:1", Volume: "snapvol"}}

	if err := ForkSpec(sp, "golden", refs); err != nil {
		t.Fatalf("ForkSpec: %v", err)
	}

	svc := sp["services"].(map[string]any)["postgres"].(map[string]any)

	if svc["image"] != "snap:1" {
		t.Errorf("image: got %v, want the snapshot's", svc["image"])
	}

	// The volume must survive. Deleting it was the first implementation's bug: the theory
	// was that the image carried the data, and `docker commit` does not capture volumes, so
	// every fork started blank.
	if svc["volume"] != "/var/lib/postgresql/data" {
		t.Errorf("the volume was dropped: %v — a fork would start with nowhere to restore into", svc)
	}

	// init has already run in the state being forked. Re-running it re-seeds a seeded
	// database: an error under a unique constraint, silent duplication without one.
	if _, ok := svc["init"]; ok {
		t.Errorf("init survived the fork: %v", svc)
	}

	// Everything not named is left alone.
	if svc["ports"] == nil {
		t.Error("ports were lost")
	}
}

func TestForkSpecRefusesAMissingService(t *testing.T) {
	sp := map[string]any{"services": map[string]any{"redis": map[string]any{"image": "redis"}}}

	err := ForkSpec(sp, "golden", []SnapshotRef{{Service: "postgres", Image: "snap:1"}})
	if err == nil {
		t.Fatal("forking a service the spec does not have was accepted")
	}
}

func TestForkSpecRefusesASpecWithNoServices(t *testing.T) {
	if err := ForkSpec(map[string]any{}, "golden", nil); err == nil {
		t.Fatal("a spec with no services was accepted")
	}
}

// Two sandboxes may each run a postgres, and two snapshots may each hold one. The names
// have to keep them apart or a fork restores somebody else's data.
func TestSnapshotNamesAreScoped(t *testing.T) {
	if snapshotVolume("a", "postgres") == snapshotVolume("b", "postgres") {
		t.Error("two snapshots share a volume name")
	}

	if snapshotImage("a", "postgres") == snapshotImage("a", "redis") {
		t.Error("two services share an image name")
	}
}
