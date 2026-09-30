package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/procid"
)

// A presence record whose pid was recycled to an unrelated process claimed a daemon that was
// gone: `sbx serve` refused to start ("already running") and create advised waiting for it.
// The record carries the daemon's start time, and a mismatch is no daemon.
func TestAPresenceRecordNamingARecycledPidIsNoDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	parent := os.Getppid()

	start, ok := procid.StartOf(parent)
	if !ok {
		t.Skip("no process start times on this platform; presence is pid-only here")
	}

	write := func(path string, p Presence) {
		t.Helper()

		body, _ := json.Marshal(p)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	machine := filepath.Join(home, ".sbx", "daemon.json")

	write(machine, Presence{PID: parent, Start: start + 1, Since: time.Now(), Provider: "docker"})

	if p, ok := Running(); ok {
		t.Fatalf("a record naming a recycled pid reads as a running daemon: %+v", p)
	}

	write(machine, Presence{PID: parent, Start: start, Since: time.Now(), Provider: "docker"})

	if _, ok := Running(); !ok {
		t.Fatal("the real daemon's record (same pid and start) reads as gone")
	}

	scoped := filepath.Join(home, ".sbx", "daemons", strconv.Itoa(parent)+".json")
	write(scoped, Presence{PID: parent, Start: start + 1, Provider: "docker", Scope: Scope{"ci-"}})

	if len(Scoped()) != 0 {
		t.Fatal("a scoped record naming a recycled pid reads as a running daemon")
	}
}

// What a daemon writes about itself carries its start time.
func TestADaemonRecordsItsStartTime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if _, ok := procid.StartOf(os.Getpid()); !ok {
		t.Skip("no process start times on this platform")
	}

	clear := MarkRunning("docker")
	defer clear()

	p, ok := Running()
	if !ok || p.Start == 0 || p.Start != procid.Self().Start {
		t.Fatalf("Running() = %+v, %v; want this process with its start time", p, ok)
	}
}
