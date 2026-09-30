// Package snapshotpause marks a container as paused by `sbx snapshot`, so the daemon can tell
// that pause from a freeze.
//
// Snapshot pauses every running service while it copies the volumes and commits the images: a
// live database copied mid-write is torn (ClickHouse merging a part made `cp` fail on a file
// that vanished under it), and a paused one is crash-consistent. The daemon sees a paused
// container on its next discovery tick and, without this, reads it as a pause done outside sbx:
// it marks the unit frozen and not awake. Snapshot then unpauses it, and the daemon is left
// believing a running container is asleep - which it never sleeps again until something
// connects. A container label cannot say "snapshot", because a label cannot be added to a
// container that exists; so the CLI leaves a file here for the length of the pause.
//
// The file holds the snapshot's pid and start time (internal/procid). A snapshot that was killed
// before it could remove it leaves a file whose process is gone - or whose pid now belongs to
// something else - and Held treats that as no mark: the daemon must never be wedged by a mark
// nobody will take back. This is slotlock's convention, for the same reason.
package snapshotpause

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/procid"
)

// Hold marks ref as paused by this process and returns how to take the mark back. It never
// fails the snapshot: a mark that cannot be written costs the daemon one wrong belief, which
// corrects on the next connection, and is no reason to leave a database unsnapshotted.
func Hold(ref string) (release func()) {
	path, err := path(ref)
	if err != nil {
		return func() {}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}
	}

	if err := os.WriteFile(path, []byte(procid.Self().String()), 0o644); err != nil {
		return func() {}
	}

	return func() { _ = os.Remove(path) }
}

// Held reports whether a live `sbx snapshot` holds ref paused. A mark whose process is gone is
// removed and reported as not held.
func Held(ref string) bool {
	path, err := path(ref)
	if err != nil {
		return false
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	rec, ok := procid.Parse(string(body))
	if !ok {
		_ = os.Remove(path)
		return false
	}

	if !rec.Alive() {
		_ = os.Remove(path)
		return false
	}

	return true
}

func path(ref string) (string, error) {
	// A ref is a container name; one with a separator in it is not ours to turn into a path.
	if ref == "" || strings.ContainsAny(ref, `/\`) || ref == "." || ref == ".." {
		return "", fmt.Errorf("snapshotpause: unusable ref %q", ref)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".sbx", "snapshot-paused", ref), nil
}
