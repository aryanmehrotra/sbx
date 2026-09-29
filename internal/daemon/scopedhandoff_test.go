package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// register writes a scoped daemon's record the way Announce does, into a fake home.
func register(t *testing.T, home string, pid int, scope ...string) string {
	t.Helper()

	dir := filepath.Join(home, ".sbx", "daemons")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(Presence{PID: pid, Since: time.Now(), Provider: "docker", Scope: scope})
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

// useRegistry makes this test read the scoped-daemon registry under its fake home.
func useRegistry(t *testing.T) {
	t.Helper()

	prev := scopedRegistry
	scopedRegistry = scopedDaemons

	t.Cleanup(func() { scopedRegistry = prev })
}

// deadPID is the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run true to get a dead pid: %v", err)
	}

	return cmd.Process.Pid
}

func adopted(d *daemon, ref string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, ok := d.units[ref]

	return ok
}

// The machine's unscoped daemon must leave a live scoped daemon's sandboxes to it.
//
// Both adopted them: `--only r2i-*` beside the machine's `sbx serve` meant two daemons racing
// to bind the same ports. Whichever lost logged "address already in use" every discovery tick,
// forever, and the winner was an accident of timing - measured live, the machine's daemon took
// the ports in the sub-second gap while the scoped one was being restarted.
//
// The claim is re-read every pass, so a scoped daemon that stops hands its sandboxes back on
// the next tick, and one that died without cleaning up (a pid that no longer exists) hides
// nothing.
func TestUnscopedDaemonLeavesALiveScopedDaemonsSandboxes(t *testing.T) {
	log.SetOutput(io.Discard)

	home := t.TempDir()
	t.Setenv("HOME", home)
	useRegistry(t)

	p := &listingProvider{units: []provider.Unit{
		{Ref: "sbx-r2i-a-s", Sandbox: "r2i-a", Service: "s", Running: true,
			Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
		{Ref: "sbx-mine-db", Sandbox: "mine", Service: "db", Running: true,
			Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := New(p, time.Hour, time.Second, time.Hour)

	// A stale record - its daemon was killed - claims nothing, and is cleared.
	stale := register(t, home, deadPID(t), "r2i-*")

	d.discover(ctx)

	if !adopted(d, "sbx-r2i-a-s") {
		t.Fatal("a dead scoped daemon's record hid its sandbox from the machine's daemon")
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale record was left behind: %v", err)
	}

	// A live scoped daemon starts: the unscoped one lets go on its next pass.
	live := register(t, home, os.Getppid(), "r2i-*")

	d.discover(ctx)

	if adopted(d, "sbx-r2i-a-s") {
		t.Fatal("the unscoped daemon still fronts a sandbox a live --only daemon covers")
	}

	if !adopted(d, "sbx-mine-db") {
		t.Fatal("a sandbox outside the scoped daemon's --only was dropped too")
	}

	// It stops (Announce's cleanup removes its record): handed back within one pass.
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond) // the dropped listener's close is asynchronous

	d.discover(ctx)

	if !adopted(d, "sbx-r2i-a-s") {
		t.Fatal("the sandbox was not fronted again after its scoped daemon stopped")
	}
}

// A scoped daemon is not deferred by its own record, nor by another scoped daemon's: overlap
// between two --only daemons is the operator's choice, and the scoped one is the one that asked.
func TestAScopedDaemonIgnoresTheRegistry(t *testing.T) {
	log.SetOutput(io.Discard)

	home := t.TempDir()
	t.Setenv("HOME", home)
	useRegistry(t)
	register(t, home, os.Getppid(), "r2i-*")

	p := &listingProvider{units: []provider.Unit{
		{Ref: "sbx-r2i-a-s", Sandbox: "r2i-a", Service: "s", Running: true,
			Listen: []int{freePort(t)}, Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := New(p, time.Hour, time.Second, time.Hour)
	d.scope = Scope{"r2i-"}
	d.discover(ctx)

	if !adopted(d, "sbx-r2i-a-s") {
		t.Fatal("a scoped daemon deferred its own sandbox to the registry")
	}
}
