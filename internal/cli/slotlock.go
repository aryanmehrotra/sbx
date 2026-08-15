package cli

// Claiming a port slot without racing another `sbx create`.
//
// AllocSlot reads the sandboxes that exist and returns the first free slot. Two creates
// running at the same moment read the same state and pick the same slot, and the ports only
// actually get claimed when a container is created — so the loser fails at `docker run` with
// "failed to set up container networking: driver failed programming external connectivity".
// Measured, four concurrent creates on one machine: three of them failed.
//
// That is not a rare shape. docs/USE-CASES.md describes a sandbox per CI job on a persistent
// runner, which is several creates arriving together by construction.
//
// The lock is held from AllocSlot until the FIRST container exists, not for the whole create.
// Once a container carries the slot label and holds the ports, every other AllocSlot can see
// it, and the rest of the work — pulls, health checks, init — happens unserialised. A create
// that pulls a two-gigabyte image must not block every other create on the machine.
//
// Advisory, and deliberately not fatal. It is a file under $HOME, so it covers one machine
// and not a shared remote DOCKER_HOST; if it cannot be taken, create proceeds and races
// exactly as it did before. A lock that can wedge a working command is worse than the race
// it prevents.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	slotLockWait  = 90 * time.Second
	slotLockRetry = 50 * time.Millisecond
)

// Within one process too, not only between them. The file lock records a pid, and a pid
// cannot distinguish two goroutines — so concurrent callers here would each see their own
// pid in the file, conclude the holder is alive, and spin, which is correct but only by
// accident. This makes the in-process case exclusive by construction and is what lets the
// property be tested at all.
var slotLockLocal sync.Mutex

// lockSlots blocks until it holds the slot lock, and returns a release function.
//
// The release is safe to call more than once, because the caller releases it early on the
// happy path and defers it for every other path.
func lockSlots() func() {
	slotLockLocal.Lock()

	path, err := slotLockPath()
	if err != nil {
		return releaseOnce(func() {})
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return releaseOnce(func() {})
	}

	deadline := time.Now().Add(slotLockWait)

	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d", os.Getpid())
			_ = f.Close()

			return releaseOnce(func() { _ = os.Remove(path) })
		}

		// Held by something. If that something is gone — a create that was killed — the lock
		// is rubbish and must not block the machine for ever.
		if clearStaleSlotLock(path) {
			continue
		}

		if time.Now().After(deadline) {
			// Proceed anyway. Waiting longer than a create takes is worse than racing.
			return releaseOnce(func() {})
		}

		time.Sleep(slotLockRetry)
	}
}

// clearStaleSlotLock removes a lock whose owner is no longer running, and reports whether it
// did.
func clearStaleSlotLock(path string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false // gone already, which the next attempt will discover
	}

	pid, err := strconv.Atoi(string(body))
	if err != nil || pid <= 0 {
		_ = os.Remove(path) // unreadable, so not something anyone can be waiting on

		return true
	}

	if pid == os.Getpid() {
		return false // ours, somehow; do not delete it underneath ourselves
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(path)

		return true
	}

	if err := proc.Signal(syscall.Signal(0)); err != nil {
		_ = os.Remove(path)

		return true
	}

	return false
}

func slotLockPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".sbx", "slots.lock"), nil
}

// releaseOnce wraps a cleanup so the in-process mutex is released exactly once, however many
// times the caller calls it. Create releases early on the happy path and defers it for every
// other path, so more than once is the normal case rather than a mistake.
func releaseOnce(cleanup func()) func() {
	var once sync.Once

	return func() {
		once.Do(func() {
			cleanup()
			slotLockLocal.Unlock()
		})
	}
}
