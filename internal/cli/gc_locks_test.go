package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/slotlock"
)

// A name lock left by a killed create was cleared only when that name was next used, which for
// a one-off name is never. `sbx gc` lists stale locks, and --force removes them; a live
// holder's lock is never offered.
func TestGCListsAndRemovesStaleLocks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dead, _ := slotlock.NamePath("killed-ci-42")
	live, _ := slotlock.NamePath("in-use")

	for path, pid := range map[string]int{dead: 1<<22 + 12345, live: os.Getppid()} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	if err := gcLocks(&out, false); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), dead) || strings.Contains(out.String(), live) {
		t.Errorf("want the dead holder's lock listed and the live one not:\n%s", out.String())
	}

	if _, err := os.Stat(dead); err != nil {
		t.Fatal("a listing run removed a lock")
	}

	out.Reset()

	if err := gcLocks(&out, true); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dead); err == nil {
		t.Errorf("--force left the stale lock:\n%s", out.String())
	}

	if _, err := os.Stat(live); err != nil {
		t.Errorf("--force removed a live holder's lock: %v", err)
	}
}
