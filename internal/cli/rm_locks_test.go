package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/slotlock"
)

// writeNameLock leaves a name lock as another process would: its pid, and its kind on a second
// line when it has one.
func writeNameLock(t *testing.T, sandbox string, pid int, kind string) string {
	t.Helper()

	path, err := slotlock.NamePath(sandbox)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	body := strconv.Itoa(pid)
	if kind != "" {
		body += "\n" + kind
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func stubWith(sandbox string) *raceStub {
	p := newRaceStub()
	p.units[sandbox] = []provider.Unit{{Sandbox: sandbox, Service: "redis", Ref: "sbx-" + sandbox + "-redis"}}

	return p
}

// A create killed part-way leaves its sandbox and its name lock. `sbx rm` took the sandbox and
// left the lock, so the lock files piled up for `sbx gc` to find. The name's lock goes with the
// sandbox when its holder is no longer running.
func TestRmRemovesTheNamesStaleLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := stubWith("killed")
	lock := writeNameLock(t, "killed", 1<<22+12345, "")

	var err error

	_ = captureOutput(t, func() { err = Rm(context.Background(), p, "killed") })
	if err != nil {
		t.Fatal(err)
	}

	if p.has("killed") {
		t.Fatal("the sandbox was not removed")
	}

	if _, err := os.Stat(lock); err == nil {
		t.Errorf("sbx rm left the stale name lock %s", lock)
	}
}

// Never a live holder's: a lock whose process is still running belongs to it.
func TestRmKeepsALiveHoldersLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := stubWith("busy")
	lock := writeNameLock(t, "busy", os.Getppid(), "")

	_ = captureOutput(t, func() { _ = Rm(context.Background(), p, "busy") })

	if _, err := os.Stat(lock); err != nil {
		t.Errorf("sbx rm removed a live holder's name lock: %v", err)
	}
}

// A running `sbx with` owns its sandbox: `sbx rm` of it went through silently, and the `with`
// then exited 0 with no word that its sandbox had vanished under its command. Create and add of
// that name are refused, naming the pid; rm is refused the same way and touches nothing.
func TestRmRefusesASandboxALiveWithOwns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := stubWith("eph")
	holder := os.Getppid()
	lock := writeNameLock(t, "eph", holder, "with")

	var err error

	_ = captureOutput(t, func() { err = Rm(context.Background(), p, "eph") })
	if err == nil {
		t.Fatal("sbx rm removed the sandbox of a running sbx with")
	}

	for _, want := range []string{"ephemeral sandbox of `sbx with`", "pid " + strconv.Itoa(holder)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}

	if !p.has("eph") || len(p.removed) != 0 {
		t.Errorf("the refused rm removed something: %v", p.removed)
	}

	if _, err := os.Stat(lock); err != nil {
		t.Errorf("the refused rm removed the with's lock: %v", err)
	}
}

// Once the `with` is gone its lock is stale, and rm goes ahead: the refusal is about a live owner,
// not about the kind written in a file.
func TestRmOfADeadWithsSandboxGoesAhead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := stubWith("orphan")
	lock := writeNameLock(t, "orphan", 1<<22+12346, "with")

	var err error

	_ = captureOutput(t, func() { err = Rm(context.Background(), p, "orphan") })
	if err != nil {
		t.Fatalf("a dead with's lock blocked sbx rm: %v", err)
	}

	if p.has("orphan") {
		t.Error("the sandbox was not removed")
	}

	if _, err := os.Stat(lock); err == nil {
		t.Error("the dead with's stale lock was left behind")
	}
}
