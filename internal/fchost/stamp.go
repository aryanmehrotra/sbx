package fchost

// Which sbx the helper VM was last brought up to date with, recorded on this side.
//
// A command against a RUNNING helper VM used to skip Ensure entirely - re-hashing the linux binary
// on every `sbx list` is a round trip into the VM - so after an upgrade the VM kept running the old
// sbx, its old daemon and its old protocol until somebody stopped it. The stamp makes the common
// case free and the upgrade case correct: it names the host build (its version and its
// executable's identity on disk), is written after every successful Ensure, and a command that
// finds the VM running re-Ensures only when this build is not the one stamped.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hostStamp names this build: version plus the executable's path, size and modification time.
// A stat, not a hash - it runs on every redirected command - and any rebuild or upgrade moves it.
func hostStamp(version string) string {
	exe, err := os.Executable()
	if err != nil {
		return version
	}

	fi, err := os.Stat(exe)
	if err != nil {
		return version + " " + exe
	}

	return fmt.Sprintf("%s %s %d %d", version, exe, fi.Size(), fi.ModTime().UnixNano())
}

func (m *Manager) stampPath() string { return filepath.Join(m.StateDir, "installed-"+m.Config.Name) }

func (m *Manager) stamp(version string) string {
	if m.hostStamp != nil {
		return m.hostStamp(version)
	}

	return hostStamp(version)
}

func (m *Manager) writeStamp(version string) {
	if err := os.MkdirAll(m.StateDir, 0o700); err == nil {
		_ = os.WriteFile(m.stampPath(), []byte(m.stamp(version)+"\n"), 0o600)
	}
}

// Current reports whether the helper VM is running AND was last ensured by this build.
func (m *Manager) Current(ctx context.Context, version string) (bool, error) {
	st, err := m.Status(ctx)
	if err != nil || st != Running {
		return false, err
	}

	b, err := os.ReadFile(m.stampPath())

	return err == nil && strings.TrimSpace(string(b)) == m.stamp(version), nil
}

// EnsureCurrent is Ensure unless the VM is running and already this build's: the cheap check a
// redirected command and Remote make before every use.
func (m *Manager) EnsureCurrent(ctx context.Context, opt EnsureOptions) error {
	ok, err := m.Current(ctx, opt.Version)
	if err != nil || ok {
		return err
	}

	return m.Ensure(ctx, opt)
}
