package fchost

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/procid"
)

// A helper-VM lock naming a pid that is alive but is not the process that took it - the holder
// died and the pid was recycled - blocked every Ensure, Stop and Remove for the fifteen-minute
// wait with nothing holding it. The lock records the holder's start time, and a mismatch is
// stale; the real holder's record, same pid and start, still holds.
func TestAHelperVMLockNamingARecycledPidIsStale(t *testing.T) {
	parent := os.Getppid()

	start, ok := procid.StartOf(parent)
	if !ok {
		t.Skip("no process start times on this platform; the VM lock is pid-only here")
	}

	m := &Manager{Config: testCfg, Out: io.Discard, StateDir: t.TempDir()}

	if err := os.WriteFile(m.lockPath(), []byte(procid.Record{PID: parent, Start: start + 1}.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	began := time.Now()

	unlock, err := m.lockVM(context.Background())
	if err != nil {
		t.Fatalf("a recycled pid's lock blocked: %v", err)
	}

	if took := time.Since(began); took > 500*time.Millisecond {
		t.Errorf("took %s to see a recycled pid's lock as stale", took)
	}

	// What this process wrote is its own record, readable back as itself.
	body, _ := os.ReadFile(m.lockPath())
	if rec, ok := procid.Parse(string(body)); !ok || rec != procid.Self() {
		t.Errorf("the lock holds %q, want this process's record %q", body, procid.Self().String())
	}

	unlock()

	saved := vmLockWait
	vmLockWait = 100 * time.Millisecond
	t.Cleanup(func() { vmLockWait = saved })

	if err := os.WriteFile(m.lockPath(), []byte(procid.Record{PID: parent, Start: start}.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.lockVM(context.Background()); err == nil || !strings.Contains(err.Error(), "pid "+strconv.Itoa(parent)) {
		t.Fatalf("the live holder's own record (same pid and start) = %v, want it held", err)
	}
}
