//go:build !windows

package cli

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/snapshotpause"
)

// Ctrl-C mid-copy: the process must live long enough to roll back and thaw, not die with the
// database frozen. The signal arrives during the first service's copy, as a terminal's would.
func TestAnInterruptedSnapshotRollsBackAndThaws(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	e := &signalEngine{engine: newEngine(units("app", "db", "web")...)}
	e.volumes["sbx-app-db-data"] = 3
	e.volumes["sbx-app-web-data"] = 2

	before := e.state()

	if _, err := Snapshot(context.Background(), e, "app", "gold"); err == nil {
		t.Fatal("an interrupted snapshot reported success")
	}

	if after := e.state(); after != before {
		t.Errorf("an interrupted snapshot left debris:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	for _, r := range []string{"sbx-app-db", "sbx-app-web"} {
		if at(e.events, "unpause "+r) < 0 || snapshotpause.Held(r) {
			t.Errorf("%s was left paused or marked: %v", r, e.events)
		}
	}
}

type signalEngine struct {
	*engine
	sent bool
}

func (s *signalEngine) CopyVolume(ctx context.Context, src, dst string) error {
	if !s.sent {
		s.sent = true
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		time.Sleep(100 * time.Millisecond) // delivery to the Notify channel is asynchronous
	}

	return s.engine.CopyVolume(ctx, src, dst)
}
