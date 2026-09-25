package osb

// The operator key when nobody passed one.
//
// v0.9.0 served a loopback --osb-addr with no key at all, on the theory that "is this yours" is
// answered by being on this machine. It is not, on the engines sbx mostly runs on: colima and
// Docker Desktop run containers in a VM whose gateway forwards to the host's 127.0.0.1
// (host.docker.internal, host.lima.internal, 192.168.5.2 on colima). So every container on the
// engine - including API sandboxes running the code they were created to run - could reach a
// keyless API, list the sandboxes, read each one's execd token from its endpoint, and run
// commands in any of them. A key is therefore required by default: generated once, kept in a file
// only this user can read, and found there by the clients on this machine.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultStateDir is where the API keeps its records and its generated key: ~/.sbx/osb.
func DefaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("osb: no home directory for the API's state (%v) - set HOME, or pass --osb-key", err)
	}

	return filepath.Join(home, ".sbx", "osb"), nil
}

// KeyFile is the generated key's path inside a state dir.
func KeyFile(dir string) string { return filepath.Join(dir, "key") }

// LoadOrCreateKey returns the key stored in dir, minting and storing one if there is none. The
// directory is made 0700 and the file 0600 - and tightened to that if they were looser, because
// the key is a credential for every API sandbox on the machine.
func LoadOrCreateKey(dir string) (key, path string, created bool, err error) {
	path = KeyFile(dir)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", path, false, fmt.Errorf("creating %s for the API key: %w", dir, err)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		return "", path, false, fmt.Errorf("restricting %s to its owner: %w", dir, err)
	}

	// O_EXCL, so two daemons starting at once cannot each write a different key and leave one of
	// them serving a key nobody can read any more.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		key = newToken() + newToken()

		_, werr := f.WriteString(key + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}

		if werr != nil {
			_ = os.Remove(path)
			return "", path, false, fmt.Errorf("writing the API key to %s: %w", path, werr)
		}

		return key, path, true, nil
	}

	if !errors.Is(err, os.ErrExist) {
		return "", path, false, fmt.Errorf("creating the API key at %s: %w", path, err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		return "", path, false, fmt.Errorf("restricting %s to its owner: %w", path, err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return "", path, false, fmt.Errorf("reading the API key from %s: %w", path, err)
	}

	// Empty is a broken file, not a request for no key: serving keyless because a file was
	// truncated would be exactly the exposure this file exists to close.
	key = strings.TrimSpace(string(body))
	if key == "" {
		return "", path, false, fmt.Errorf("the API key file %s is empty - remove it and restart "+
			"sbx serve to generate a new one, or pass --osb-key", path)
	}

	return key, path, false, nil
}

// ReadKey is the client side: the key in dir, or "" when there is none or it cannot be read.
// It never creates one - only the server decides there is a key.
func ReadKey(dir string) string {
	body, err := os.ReadFile(KeyFile(dir))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(body))
}
