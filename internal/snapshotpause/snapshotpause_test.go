package snapshotpause

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHoldAndRelease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if Held("sbx-a-db") {
		t.Fatal("held before Hold")
	}

	release := Hold("sbx-a-db")

	if !Held("sbx-a-db") {
		t.Fatal("not held after Hold")
	}

	if Held("sbx-a-web") {
		t.Fatal("Hold of one ref marked another")
	}

	release()

	if Held("sbx-a-db") {
		t.Fatal("still held after release")
	}
}

// A snapshot killed while it held a pause leaves its file; the daemon must not honour it for
// ever. A pid that is not running is no mark, and the file is cleared.
func TestAMarkWhoseProcessIsGoneIsNotHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".sbx", "snapshot-paused")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Well above any pid_max, so no process has it.
	if err := os.WriteFile(filepath.Join(dir, "sbx-a-db"), []byte("2147483646"), 0o644); err != nil {
		t.Fatal(err)
	}

	if Held("sbx-a-db") {
		t.Fatal("a dead snapshot's mark is honoured")
	}

	if _, err := os.Stat(filepath.Join(dir, "sbx-a-db")); err == nil {
		t.Fatal("the dead mark was not cleared")
	}
}

func TestARefThatIsAPathIsNeverHeld(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	defer Hold("../x")()

	if Held("../x") {
		t.Fatal("a ref with a separator was turned into a path")
	}
}
