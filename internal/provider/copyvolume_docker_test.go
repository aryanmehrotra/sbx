package provider

// The one primitive in this project that moves user data, exercised against a real docker.
//
// Skipped where there is no daemon rather than failing, so `go test ./...` still means
// something on a machine without one — and so that this exists at all, which it did not.
// DECISIONS.md records the failure this guards: a fork that came up with a working server and
// an empty database, "the worst shape of failure, because it looks like it worked".

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func dockerOrSkip(t *testing.T) *dockerProvider {
	t.Helper()

	// Each of these starts several containers, so the set costs the better part of a minute.
	// Worth it in CI and not worth it on every local `go test`, which is what -short is for.
	if testing.Short() {
		t.Skip("skipped under -short: this starts real containers")
	}

	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no docker daemon here")
	}

	ep, err := resolveDockerHost("")
	if err != nil {
		t.Skipf("cannot resolve a docker endpoint: %v", err)
	}

	return newDocker(ep)
}

func volume(t *testing.T, d *dockerProvider, name string, files int) {
	t.Helper()

	t.Cleanup(func() { _, _ = d.docker("volume", "rm", "-f", name) })

	script := "mkdir -p /v/dir"
	for i := range files {
		script += "; echo content" + string(rune('a'+i)) + " > /v/dir/f" + string(rune('a'+i))
	}

	if out, err := d.docker("run", "--rm", "-v", name+":/v", "alpine:3", "sh", "-c", script); err != nil {
		t.Fatalf("seeding %s: %v: %s", name, err, out)
	}
}

func countFiles(t *testing.T, d *dockerProvider, name string) int {
	t.Helper()

	out, err := d.docker("run", "--rm", "-v", name+":/v", "alpine:3",
		"sh", "-c", "find /v -type f | wc -l")
	if err != nil {
		t.Fatalf("counting %s: %v", name, err)
	}

	n := 0
	for _, f := range strings.Fields(out) {
		if v, err := atoiSafe(f); err == nil {
			n = v
		}
	}

	return n
}

func atoiSafe(s string) (int, error) {
	n := 0

	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errNotNumber
		}

		n = n*10 + int(r-'0')
	}

	return n, nil
}

var errNotNumber = &notNumber{}

type notNumber struct{}

func (*notNumber) Error() string { return "not a number" }

func TestCopyVolumeMovesEveryFile(t *testing.T) {
	d := dockerOrSkip(t)

	src, dst := "sbx-cvtest-src", "sbx-cvtest-dst"
	volume(t, d, src, 3)

	t.Cleanup(func() { _, _ = d.docker("volume", "rm", "-f", dst) })

	if err := d.CopyVolume(context.Background(), src, dst); err != nil {
		t.Fatalf("CopyVolume: %v", err)
	}

	if got := countFiles(t, d, dst); got != 3 {
		t.Errorf("destination has %d files, want 3", got)
	}
}

// Docker creates an empty volume for a name that does not exist rather than failing, so a
// copy from a snapshot that was never made used to be a silent success producing nothing —
// and worse, producing it by first deleting whatever the destination held.
//
// Reachable: `sbx gc --snapshots --force` collects a snapshot's image and its volume as two
// separate artifacts, so an interrupted sweep leaves an image SnapshotsOf still resolves with
// no volume behind it.
func TestCopyVolumeRefusesAnEmptySource(t *testing.T) {
	d := dockerOrSkip(t)

	dst := "sbx-cvtest-dst2"
	volume(t, d, dst, 2) // freshly initialised, as Create leaves it

	t.Cleanup(func() { _, _ = d.docker("volume", "rm", "-f", "sbx-cvtest-nothing-here") })

	err := d.CopyVolume(context.Background(), "sbx-cvtest-nothing-here", dst)
	if err == nil {
		t.Fatal("reported success copying from a source that does not exist")
	}

	// And it refused before touching anything: the destination still holds what it held.
	if got := countFiles(t, d, dst); got != 2 {
		t.Errorf("the destination has %d files, want the 2 it started with — it was wiped "+
			"before the copy was found to be impossible", got)
	}
}

// A restore replaces. Merging leaves files from the freshly initialised data directory inside
// what is meant to be an exact copy of the snapshot.
func TestCopyVolumeReplacesRatherThanMerges(t *testing.T) {
	d := dockerOrSkip(t)

	src, dst := "sbx-cvtest-src3", "sbx-cvtest-dst3"
	volume(t, d, src, 2)
	volume(t, d, dst, 5) // as if Create had initialised it

	if err := d.CopyVolume(context.Background(), src, dst); err != nil {
		t.Fatalf("CopyVolume: %v", err)
	}

	if got := countFiles(t, d, dst); got != 2 {
		t.Errorf("destination has %d files, want exactly the source's 2 — the copy merged "+
			"over the destination instead of replacing it", got)
	}
}
