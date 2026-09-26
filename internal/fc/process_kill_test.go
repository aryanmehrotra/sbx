package fc

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// stubProcs replaces the process table Kill reads for the length of a test.
func stubProcs(t *testing.T, owns func(int, string) bool, holding func(int, uint64) bool, kill func(int) error) {
	t.Helper()

	o, h, k := ownsProc, holdingProc, killProc
	ownsProc, holdingProc, killProc = owns, holding, kill

	t.Cleanup(func() { ownsProc, holdingProc, killProc = o, h, k })
}

// A VMM's command line is its own to overwrite: a compromised one can erase the socket and --id
// that owns() looks for. The pid and its start time are the kernel's, so a process that still
// matches both is the one Launch started, and it is killed - not waited on for ever, which left
// Remove stuck on a VM that would never exit.
func TestKillEndsTheRecordedProcessEvenWhenItsArgvNoLongerNamesTheVM(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PIDName), []byte("4242 777\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var killed atomic.Int32

	stubProcs(t,
		func(int, string) bool { return false }, // argv overwritten
		func(pid int, start uint64) bool { return pid == 4242 && start == 777 && killed.Load() == 0 },
		func(pid int) error {
			if pid == 4242 {
				killed.Add(1)
			}

			return nil
		})

	if err := (ExecLauncher{}).Kill(context.Background(), dir); err != nil {
		t.Fatalf("Kill = %v", err)
	}

	if killed.Load() != 1 {
		t.Fatalf("SIGKILLs sent to the recorded process = %d, want 1", killed.Load())
	}

	if _, err := os.Stat(filepath.Join(dir, PIDName)); !os.IsNotExist(err) {
		t.Fatalf("the pid file outlived the process: %v", err)
	}
}

// A pid whose start time is no longer the recorded one is somebody else's: never signalled.
func TestKillLeavesAReusedPidAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PIDName), []byte("4242 777\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stubProcs(t,
		func(int, string) bool { return false },
		func(int, uint64) bool { return false },
		func(pid int) error { t.Fatalf("signalled pid %d, which is no longer ours", pid); return nil })

	if err := (ExecLauncher{}).Kill(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
}
