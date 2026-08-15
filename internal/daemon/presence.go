package daemon

// Whether a daemon is running on this machine.
//
// Everything sbx exports is a port the daemon answers on, so "is there a daemon?" is the
// question behind most confusing first runs: the sandbox is created, `sbx env` prints an
// address, and the connection is refused because nothing is fronting it. Answering that by
// dialling a port cannot distinguish "no daemon" from "the daemon has not noticed this
// sandbox yet", and those need opposite advice — start one, versus wait a moment.
//
// So the daemon says so directly, in a file, with its pid. A pid is checkable: a file left
// behind by a killed daemon is detected as stale rather than believed.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Presence is what a running daemon writes about itself.
type Presence struct {
	PID      int       `json:"pid"`
	Since    time.Time `json:"since"`
	Provider string    `json:"provider"`
}

// presencePath is under $HOME rather than /var/run: the daemon runs as you, not as root,
// and a per-user path is what makes two people on one box not fight over one file.
func presencePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".sbx", "daemon.json"), nil
}

// MarkRunning records that this process is the daemon, and returns a function that clears it.
//
// Best-effort throughout. A daemon that cannot write this file still works — every part of
// sbx that matters dials ports, not this — so a read-only home directory degrades the advice
// in error messages rather than stopping the daemon from running.
func MarkRunning(providerName string) func() {
	path, err := presencePath()
	if err != nil {
		return func() {}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}
	}

	body, err := json.Marshal(Presence{PID: os.Getpid(), Since: time.Now(), Provider: providerName})
	if err != nil {
		return func() {}
	}

	if err := os.WriteFile(path, body, 0o644); err != nil {
		return func() {}
	}

	return func() {
		// Only clear it if it is still ours. A second daemon that replaced this file should
		// not have its presence deleted by the first one exiting.
		if p, ok := Running(); ok && p.PID == os.Getpid() {
			_ = os.Remove(path)
		}
	}
}

// Running reports the daemon on this machine, if there is one.
//
// A stale file — written by a daemon that was killed rather than stopped — is the common
// case on a laptop, so the pid is verified rather than trusted. Signal 0 is the portable
// "does this process exist" question and delivers nothing.
func Running() (Presence, bool) {
	path, err := presencePath()
	if err != nil {
		return Presence{}, false
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return Presence{}, false
	}

	var p Presence
	if json.Unmarshal(body, &p) != nil || p.PID <= 0 {
		return Presence{}, false
	}

	proc, err := os.FindProcess(p.PID)
	if err != nil {
		return Presence{}, false
	}

	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Gone. Clear it so the next reader does not have to work this out again.
		_ = os.Remove(path)

		return Presence{}, false
	}

	return p, true
}
