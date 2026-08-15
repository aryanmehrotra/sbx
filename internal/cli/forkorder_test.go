package cli

// Fork restores a snapshot's volume over a service that Create just started, and the order
// is the whole correctness argument.
//
// Create starts every service to health-check it. For a database that means it initialised
// an empty data directory and is now serving from it. Copying the snapshot over that while
// it runs is replacing the floor underneath a live process — and DECISIONS.md already
// records what this class of bug looks like here: a fork that came up with a healthy server
// and an empty database, which is the worst shape of failure because it looks like it worked.
//
// scripts/fork-e2e.sh proves the end result against real docker, but it would not localise
// an ordering regression and it does not run under `go test`.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// recorder is a Provider and a Snapshotter that writes down what it was asked to do.
type recorder struct {
	provider.Provider

	calls   []string
	stopErr error
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) Stop(_ context.Context, ref string) error {
	if r.stopErr != nil {
		return r.stopErr
	}

	r.calls = append(r.calls, "stop "+ref)

	return nil
}

func (r *recorder) CopyVolume(_ context.Context, src, dst string) error {
	r.calls = append(r.calls, "copy "+src+" -> "+dst)

	return nil
}

func (r *recorder) VolumeFor(sandbox, service string) string {
	return "sbx-" + sandbox + "-" + service
}

func (r *recorder) Commit(context.Context, string, string) error     { return nil }
func (r *recorder) Images(context.Context, string) ([]string, error) { return nil, nil }

// The invariant: for every service whose data is restored, the stop of that service appears
// before the copy into it.
func TestRestoreStopsEachServiceBeforeWritingOverIt(t *testing.T) {
	r := &recorder{}

	units := []provider.Unit{
		{Service: "postgres", Ref: "sbx-fork-postgres"},
		{Service: "redis", Ref: "sbx-fork-redis"},
	}

	refs := []SnapshotRef{
		{Service: "postgres", Volume: "sbx-snapvol-golden-postgres"},
		{Service: "redis", Volume: "sbx-snapvol-golden-redis"},
	}

	if err := restoreVolumes(context.Background(), r, r, "fork", units, refs); err != nil {
		t.Fatalf("restoreVolumes: %v", err)
	}

	for _, svc := range []string{"postgres", "redis"} {
		stop := indexOf(r.calls, "stop sbx-fork-"+svc)
		copyc := indexOf(r.calls, "copy sbx-snapvol-golden-"+svc+" -> sbx-fork-"+svc)

		if stop < 0 {
			t.Errorf("%s was never stopped before its data was replaced", svc)
			continue
		}

		if copyc < 0 {
			t.Errorf("%s's data was never restored", svc)
			continue
		}

		if stop > copyc {
			t.Errorf("%s: copied at %d but stopped at %d — the snapshot was written over a "+
				"running service\n  sequence: %v", svc, copyc, stop, r.calls)
		}
	}
}

// A service the snapshot knows nothing about is left exactly as Create made it — not
// stopped, not overwritten. A fork that stopped services it had no data for would hand back
// a sandbox with half of it down.
func TestRestoreLeavesUnsnapshottedServicesAlone(t *testing.T) {
	r := &recorder{}

	units := []provider.Unit{
		{Service: "postgres", Ref: "sbx-fork-postgres"},
		{Service: "chrome", Ref: "sbx-fork-chrome"},
	}

	refs := []SnapshotRef{{Service: "postgres", Volume: "sbx-snapvol-golden-postgres"}}

	if err := restoreVolumes(context.Background(), r, r, "fork", units, refs); err != nil {
		t.Fatalf("restoreVolumes: %v", err)
	}

	for _, c := range r.calls {
		if strings.Contains(c, "chrome") {
			t.Errorf("touched a service with no snapshotted data: %q", c)
		}
	}
}

// If the stop fails, the copy must not happen anyway. Carrying on would write over a running
// data directory, which is the exact failure the ordering exists to prevent.
func TestRestoreDoesNotCopyWhenTheStopFailed(t *testing.T) {
	r := &recorder{stopErr: errors.New("container is wedged")}

	units := []provider.Unit{{Service: "postgres", Ref: "sbx-fork-postgres"}}
	refs := []SnapshotRef{{Service: "postgres", Volume: "sbx-snapvol-golden-postgres"}}

	err := restoreVolumes(context.Background(), r, r, "fork", units, refs)
	if err == nil {
		t.Fatal("a failed stop was ignored")
	}

	for _, c := range r.calls {
		if strings.HasPrefix(c, "copy") {
			t.Errorf("copied after a failed stop: %q", c)
		}
	}
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}

	return -1
}
