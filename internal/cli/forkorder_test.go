package cli

// A fork lays the snapshot's data down BEFORE anything can run on it, and that order is the
// whole correctness argument.
//
// It used to be the other way round: Create started every service to health-check it, so a
// database initialised an empty data directory and served from it, and only then was the
// snapshot copied over the top. Between those two steps the fork existed, was healthy, and had
// the wrong data - and `kill -9` anywhere in that window left it that way. scripts/interrupt-
// e2e.sh states the invariant as "a fork that ends up present is either correct or cleanly
// gone", and it failed intermittently, in CI and locally, on two different rounds.
//
// Filling the volumes first closes it from both sides. Interrupted during the copy, no sandbox
// exists yet. Interrupted during Create, every service that exists already has the snapshot's
// data under it.
//
// The end-to-end proof is scripts/interrupt-e2e.sh against real docker, which kills the CLI
// mid-operation; these localise a regression in the copy step itself, which that script would
// only report as a wrong answer somewhere.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// recorder is a Snapshotter that writes down what it was asked to do.
type recorder struct {
	provider.Provider

	calls   []string
	copyErr error
}

func (r *recorder) Name() string { return "recorder" }

// Kept so that a Stop reaching this code would be recorded rather than silently ignored. The
// restore no longer takes a Provider at all, so this should never fire - see the test below.
func (r *recorder) Stop(_ context.Context, ref string) error {
	r.calls = append(r.calls, "stop "+ref)

	return nil
}

func (r *recorder) CopyVolume(_ context.Context, src, dst string) error {
	if r.copyErr != nil {
		return r.copyErr
	}

	r.calls = append(r.calls, "copy "+src+" -> "+dst)

	return nil
}

func (r *recorder) VolumeFor(sandbox, service string) string {
	return "sbx-" + sandbox + "-" + service
}

func (r *recorder) Commit(context.Context, string, string) error     { return nil }
func (r *recorder) Images(context.Context, string) ([]string, error) { return nil, nil }

// Every snapshotted service's data is written to the name its service will be created with,
// and nothing is stopped - because at this point nothing has been started.
func TestRestoreWritesEveryVolumeAndStopsNothing(t *testing.T) {
	r := &recorder{}

	refs := []SnapshotRef{
		{Service: "postgres", Volume: "sbx-snapvol-golden-postgres"},
		{Service: "redis", Volume: "sbx-snapvol-golden-redis"},
	}

	if err := restoreVolumes(context.Background(), r, "fork", refs); err != nil {
		t.Fatalf("restoreVolumes: %v", err)
	}

	for _, svc := range []string{"postgres", "redis"} {
		want := "copy sbx-snapvol-golden-" + svc + " -> sbx-fork-" + svc
		if indexOf(r.calls, want) < 0 {
			t.Errorf("%s's data was never restored\n  sequence: %v", svc, r.calls)
		}
	}

	for _, c := range r.calls {
		if strings.HasPrefix(c, "stop") {
			t.Errorf("something was stopped during a restore that runs before anything is "+
				"created: %q", c)
		}
	}
}

// A snapshot entry with no volume behind it is skipped rather than copied from nothing.
//
// Reachable in practice: `sbx gc --snapshots --force` collects a snapshot's image and its
// volume as two separate artifacts, so an interrupted sweep leaves an image that SnapshotsOf
// still resolves with no volume behind it.
func TestRestoreSkipsASnapshotWithNoVolume(t *testing.T) {
	r := &recorder{}

	refs := []SnapshotRef{
		{Service: "postgres", Volume: "sbx-snapvol-golden-postgres"},
		{Service: "chrome", Volume: ""},
	}

	if err := restoreVolumes(context.Background(), r, "fork", refs); err != nil {
		t.Fatalf("restoreVolumes: %v", err)
	}

	for _, c := range r.calls {
		if strings.Contains(c, "chrome") {
			t.Errorf("copied from a snapshot with no volume: %q", c)
		}
	}
}

// A failed copy stops the fork rather than letting Create run on top of half-written data.
// Carrying on is how a sandbox comes up healthy with data that is neither the snapshot's nor
// a fresh one's.
func TestRestoreFailsTheForkWhenACopyFails(t *testing.T) {
	r := &recorder{copyErr: errors.New("no space left on device")}

	refs := []SnapshotRef{{Service: "postgres", Volume: "sbx-snapvol-golden-postgres"}}

	err := restoreVolumes(context.Background(), r, "fork", refs)
	if err == nil {
		t.Fatal("a failed copy was ignored, so Create would run on top of it")
	}

	if !strings.Contains(err.Error(), "postgres") {
		t.Errorf("the error does not say which service failed: %v", err)
	}
}

func indexOf(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}

	return -1
}
