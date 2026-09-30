package snapshotpause

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/procid"
)

// A pause mark whose pid was recycled to an unrelated process kept the daemon treating a
// container as paused by a snapshot that had ended. The mark carries the snapshot's start
// time, and a mismatch is no mark.
func TestAMarkNamingARecycledPidIsNotHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	parent := os.Getppid()

	start, ok := procid.StartOf(parent)
	if !ok {
		t.Skip("no process start times on this platform; marks are pid-only here")
	}

	dir := filepath.Join(home, ".sbx", "snapshot-paused")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	mark := filepath.Join(dir, "sbx-a-db")

	if err := os.WriteFile(mark, []byte(procid.Record{PID: parent, Start: start + 1}.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	if Held("sbx-a-db") {
		t.Fatal("a mark naming a recycled pid is honoured")
	}

	if err := os.WriteFile(mark, []byte(procid.Record{PID: parent, Start: start}.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	if !Held("sbx-a-db") {
		t.Fatal("a live snapshot's mark (same pid and start) is not honoured")
	}
}
