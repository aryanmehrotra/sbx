package osb

// What the API remembers about a sandbox that docker does not.
//
// Most of a sandbox's truth is already somewhere: whether it exists, which port it is on and
// whether it is running are docker labels and container state, and the daemon reads those. What
// is left is the API's own vocabulary - metadata, extensions, when it expires, the access token,
// whether it was paused on purpose - and that has to survive `sbx serve` restarting, or every
// sandbox would lose its expiry and become immortal the first time the daemon was upgraded.
//
// One JSON file per sandbox, under ~/.sbx/osb. A file rather than a database because a database
// is a dependency, and one per sandbox because the common operation is "change this one" - a
// single file for all of them would be rewritten whole on every renew.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// The states the API reports, spelled exactly as OpenSandbox's SandboxState enum.
const (
	statePending    = "Pending"
	stateRunning    = "Running"
	statePausing    = "Pausing"
	statePaused     = "Paused"
	stateResuming   = "Resuming"
	stateStopping   = "Stopping"
	stateTerminated = "Terminated"
	stateFailed     = "Failed"
)

// record is one sandbox created through the API.
type record struct {
	ID    string `json:"id"`
	Image string `json:"image"`

	// Entrypoint is the user's command, not the one the container runs: the container runs
	// `sbx execd -- <this>`, and the API reports what was asked for.
	Entrypoint []string          `json:"entrypoint"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Extensions map[string]string `json:"extensions,omitempty"`
	Platform   *platformSpec     `json:"platform,omitempty"`

	// Ports are the container ports in the order they were declared: execd's first, then any
	// the caller asked to have fronted directly (extensions["sbx.ports"]).
	Ports []int `json:"ports"`

	// Token is execd's access token. It is a credential for everything inside the sandbox,
	// which is why this file is 0600.
	Token string `json:"token"`

	// EgressToken authenticates the sidecar-shaped egress policy route (endpoints/18080). It is
	// minted separately from Token and never placed in the container - not env, files or labels -
	// because the workload holds Token, and a workload that could reach its own policy route with
	// it could lift its own egress filter. Empty on a record from before v0.9.1 until first asked
	// for.
	EgressToken string `json:"egressToken,omitempty"`

	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`

	State            string    `json:"state"`
	Reason           string    `json:"reason,omitempty"`
	Message          string    `json:"message,omitempty"`
	LastTransitionAt time.Time `json:"lastTransitionAt"`

	// PausedByAPI is a pause somebody asked for, as opposed to the daemon freezing an idle
	// sandbox. Only this one is reported as Paused and refuses traffic.
	PausedByAPI bool `json:"pausedByApi,omitempty"`

	// SnapshotID and TemplateID say what the sandbox was created from, so a snapshot or a
	// template still in use can refuse to be deleted from under it.
	SnapshotID string `json:"snapshotId,omitempty"`
	TemplateID string `json:"templateId,omitempty"`

	// OwnedVolumes are pvc volumes created for this sandbox with deleteOnSandboxTermination:
	// removed with it. Never a volume that existed before the create.
	OwnedVolumes []string `json:"ownedVolumes,omitempty"`
}

func (r *record) transition(state, reason, message string, now time.Time) {
	r.State, r.Reason, r.Message, r.LastTransitionAt = state, reason, message, now
}

// idPattern is checked before an id is ever turned into a path. Ids arrive in URLs, and one
// spelled "../../x" must be a 404 rather than a file somewhere else.
var idPattern = regexp.MustCompile(`^osb-[0-9a-f]{12}$`)

func validID(id string) bool { return idPattern.MatchString(id) }

// store is the directory of records.
type store struct{ dir string }

func (s store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// save writes atomically: a daemon killed mid-write must leave the old record or the new one,
// never half of one - a half-written record is a sandbox whose expiry nobody can read.
func (s store) save(r *record) error {
	if !validID(r.ID) {
		return fmt.Errorf("refusing to save a record with id %q", r.ID)
	}

	return writeAtomic(s.dir, r.ID, r)
}

// writeAtomic writes v as dir/id.json through a temp file and a rename, 0600. The caller has
// already checked id against its pattern.
func writeAtomic(dir, id string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, id+".*.tmp")
	if err != nil {
		return err
	}

	name := tmp.Name()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)

		return err
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}

	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return err
	}

	return os.Rename(name, filepath.Join(dir, id+".json"))
}

func (s store) remove(id string) error {
	if !validID(id) {
		return nil
	}

	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}

// all reads every record. One that cannot be parsed is skipped and reported rather than
// failing the lot: a daemon that refuses to start because one file is corrupt takes every
// other sandbox's expiry down with it.
func (s store) all() ([]*record, []error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, []error{err}
	}

	var (
		out  []*record
		errs []error
	)

	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".json" || !validID(name[:len(name)-len(".json")]) {
			continue
		}

		body, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			errs = append(errs, err)
			continue
		}

		var r record
		if err := json.Unmarshal(body, &r); err != nil || !validID(r.ID) {
			errs = append(errs, fmt.Errorf("%s: not a sandbox record (%v) - move it aside", name, err))
			continue
		}

		out = append(out, &r)
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}

		return out[i].ID < out[j].ID
	})

	return out, errs
}
