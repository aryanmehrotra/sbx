package daemon

// Whether a daemon is running on this machine.
//
// Everything sbx exports is a port the daemon answers on, so "is there a daemon?" is the
// question behind most confusing first runs: the sandbox is created, `sbx env` prints an
// address, and the connection is refused because nothing is fronting it. Answering that by
// dialling a port cannot distinguish "no daemon" from "the daemon has not noticed this
// sandbox yet", and those need opposite advice - start one, versus wait a moment.
//
// So the daemon says so directly, in a file, with its pid. A pid is checkable: a file left
// behind by a killed daemon is detected as stale rather than believed.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Presence is what a running daemon writes about itself.
type Presence struct {
	PID      int       `json:"pid"`
	Since    time.Time `json:"since"`
	Provider string    `json:"provider"`

	// Scope is the daemon's --only, empty for the machine's unscoped daemon.
	Scope Scope `json:"scope,omitempty"`
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
// Best-effort throughout. A daemon that cannot write this file still works - every part of
// sbx that matters dials ports, not this - so a read-only home directory degrades the advice
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
		// Only clear it if it is still ours. `sbx serve` refuses to start while another is
		// running, so this should not arise - but a record left by a killed daemon can be
		// claimed by the next one, and that one exiting must not delete a third's.
		if p, ok := Running(); ok && p.PID == os.Getpid() {
			_ = os.Remove(path)
		}
	}
}

// Running reports the daemon on this machine, if there is one.
//
// A stale file - written by a daemon that was killed rather than stopped - is the common
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

// Announce records this process as a running daemon and returns a function that clears it.
//
// The machine's daemon claims the one record `sbx serve` guards on. A daemon started with --only
// does not - it is allowed beside the machine's, and claiming that record would make the next
// unscoped `sbx serve` refuse - so it writes its own, keyed by pid and carrying its scope, under
// ~/.sbx/daemons. Without that it was invisible: every CLI verb that asks whether a daemon fronts
// a sandbox (create's advice, list and ui's warning, doctor, egress) answered "no sbx serve is
// running" about sandboxes a scoped daemon was fronting, waking and sleeping.
func Announce(providerName string, scope Scope) func() {
	if len(scope) == 0 {
		return MarkRunning(providerName)
	}

	dir, err := scopedDir()
	if err != nil {
		return func() {}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}
	}

	body, err := json.Marshal(Presence{PID: os.Getpid(), Since: time.Now(), Provider: providerName, Scope: scope})
	if err != nil {
		return func() {}
	}

	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return func() {}
	}

	// Keyed by our own pid, so it cannot be another daemon's to remove.
	return func() { _ = os.Remove(path) }
}

func scopedDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".sbx", "daemons"), nil
}

// Serving reports the daemon that fronts one sandbox: the machine's, or a scoped one whose
// --only matches the name. Pid-verified like Running, and a stale scoped record is removed.
func Serving(sandbox string) (Presence, bool) {
	if p, ok := Running(); ok {
		return p, true
	}

	for _, p := range scopedDaemons() {
		if p.Scope.Match(sandbox) {
			return p, true
		}
	}

	return Presence{}, false
}

// Scoped lists the live daemons started with --only.
func Scoped() []Presence { return scopedDaemons() }

func scopedDaemons() []Presence {
	dir, err := scopedDir()
	if err != nil {
		return nil
	}

	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))

	var out []Presence

	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var p Presence
		// An empty scope here would match every sandbox; only a record that names one counts.
		if json.Unmarshal(body, &p) != nil || p.PID <= 0 || len(p.Scope) == 0 {
			continue
		}

		if !alive(p.PID) {
			_ = os.Remove(path)
			continue
		}

		out = append(out, p)
	}

	return out
}

func alive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	return proc.Signal(syscall.Signal(0)) == nil
}
