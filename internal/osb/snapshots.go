package osb

// Snapshots: POST /sandboxes/{id}/snapshots, GET /snapshots, GET|DELETE /snapshots/{id}.
//
// An API sandbox keeps its state in its container's filesystem - there is no data volume, the
// image is somebody's python:3.11 and everything the caller did is in the writable layer - so
// here a snapshot IS `docker commit`, to sbx-osb-snap:<snapshot id>. That is the opposite of
// `sbx snapshot`, which copies a volume because that is where a spec sandbox's state lives;
// DECISIONS.md ("An API snapshot is the container, because that is where its state is") has
// why both are right. Volumes mounted with `volumes` are NOT in a snapshot, exactly as upstream.
//
// Commit pauses the container for the copy (docker's default), which is the "may temporarily
// pause the sandbox" the spec allows; it is resumed by docker afterwards, and a sandbox that
// was already frozen by the idle policy stays as it was.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

const (
	snapCreating = "Creating"
	snapReady    = "Ready"
	snapFailed   = "Failed"
	snapDeleting = "Deleting"

	// snapRepo is the image repository snapshots are committed to, tagged with their id. Not
	// sbx-snap-*: `sbx gc --snapshots` sweeps those as the CLI's own, and an API snapshot is
	// meant to outlive its sandbox until somebody DELETEs it.
	snapRepo = "sbx-osb-snap"
)

// defaultRestoreEntrypoint is what a sandbox restored from a snapshot runs when the create
// names none - the spec's own default. Not the image's: a committed image's ENTRYPOINT is the
// execd wrapper the source ran under, and running that as the workload would nest one agent
// inside another.
var defaultRestoreEntrypoint = []string{"tail", "-f", "/dev/null"}

var snapIDPattern = regexp.MustCompile(`^snap-[0-9a-f]{12}$`)

type snapshotRecord struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandboxId"`
	Name      string `json:"name,omitempty"`

	// Image is what a restore runs: sbx-osb-snap:<id>.
	Image    string        `json:"image"`
	Platform *platformSpec `json:"platform,omitempty"`

	State            string    `json:"state"`
	Reason           string    `json:"reason,omitempty"`
	Message          string    `json:"message,omitempty"`
	LastTransitionAt time.Time `json:"lastTransitionAt"`
	CreatedAt        time.Time `json:"createdAt"`
}

func (r *snapshotRecord) transition(state, reason, msg string, now time.Time) {
	r.State, r.Reason, r.Message, r.LastTransitionAt = state, reason, msg, now
}

type snapshotStatusJSON struct {
	State            string     `json:"state"`
	Reason           string     `json:"reason,omitempty"`
	Message          string     `json:"message,omitempty"`
	LastTransitionAt *time.Time `json:"lastTransitionAt,omitempty"`
}

type snapshotJSON struct {
	ID        string             `json:"id"`
	SandboxID string             `json:"sandboxId"`
	Name      string             `json:"name,omitempty"`
	Status    snapshotStatusJSON `json:"status"`
	CreatedAt time.Time          `json:"createdAt"`
}

type listSnapshotsJSON struct {
	Items      []snapshotJSON `json:"items"`
	Pagination paginationJSON `json:"pagination"`
}

func (r snapshotRecord) render() snapshotJSON {
	at := r.LastTransitionAt

	return snapshotJSON{ID: r.ID, SandboxID: r.SandboxID, Name: r.Name, CreatedAt: r.CreatedAt,
		Status: snapshotStatusJSON{State: r.State, Reason: r.Reason, Message: r.Message, LastTransitionAt: &at}}
}

func (s *Server) snapDir() string { return filepath.Join(s.store.dir, "snapshots") }

func newSnapID() string { return "snap-" + newID()[len("osb-"):] }

// setSnap updates a snapshot record under the lock and persists it.
func (s *Server) setSnap(id string, f func(r *snapshotRecord)) (snapshotRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.snaps[id]
	if !ok {
		return snapshotRecord{}, false
	}

	f(r)

	if err := writeAtomic(s.snapDir(), r.ID, r); err != nil {
		logs.Default.Error(r.SandboxID, "", "osb: could not persist snapshot %s: %v", r.ID, err)
	}

	return *r, true
}

func (s *Server) snap(id string) (snapshotRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.snaps[id]
	if !ok {
		return snapshotRecord{}, false
	}

	return *r, true
}

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}

	snapper, err := provider.SnapshotterFor(s.p)
	if err != nil {
		writeErr(w, http.StatusNotImplemented, "SANDBOX::API_NOT_SUPPORTED", err.Error())
		return
	}

	var body struct {
		Name *string `json:"name"`
	}

	raw, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()

		if err := dec.Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER",
				`the body must be empty or {"name": "<snapshot name>"}: `+err.Error())

			return
		}
	}

	if body.Name != nil && strings.TrimSpace(*body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", "name, when given, must not be empty")
		return
	}

	units, err := s.unitsBySandbox(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SANDBOX::INTERNAL_ERROR", err.Error())
		return
	}

	// The spec requires Running. Idle-frozen and idle-stopped count, as they do everywhere
	// else here: both are Running to the caller, and docker commits either.
	if st := liveStatus(rec, units[rec.ID]); st.State != stateRunning {
		writeErr(w, http.StatusConflict, "SNAPSHOT::INVALID_SOURCE_STATE",
			fmt.Sprintf("%s is %s; only a Running sandbox can be snapshotted%s", rec.ID, st.State,
				map[bool]string{true: " - POST .../resume first", false: ""}[st.State == statePaused]))

		return
	}

	now := s.now().UTC()
	id := newSnapID()
	sr := &snapshotRecord{
		ID: id, SandboxID: rec.ID, Image: snapRepo + ":" + id, Platform: rec.Platform,
		CreatedAt: now, State: snapCreating, Reason: "snapshot_accepted",
		Message: "committing the sandbox's filesystem", LastTransitionAt: now,
	}

	if body.Name != nil {
		sr.Name = *body.Name
	}

	s.mu.Lock()
	s.snaps[id] = sr

	if err := writeAtomic(s.snapDir(), id, sr); err != nil {
		delete(s.snaps, id)
		s.mu.Unlock()

		writeErr(w, http.StatusInternalServerError, "SANDBOX::INTERNAL_ERROR",
			fmt.Sprintf("could not record the snapshot under %s: %v", s.snapDir(), err))

		return
	}

	resp := sr.render()
	s.mu.Unlock()

	ref := ""
	if us := units[rec.ID]; len(us) > 0 {
		ref = us[0].Ref
	}

	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		s.capture(s.base, snapper, id, rec.ID, ref)
	}()

	w.Header().Set("Location", "/v1/snapshots/"+id)
	writeJSON(w, http.StatusAccepted, resp)
}

// capture does the commit and records the outcome.
func (s *Server) capture(ctx context.Context, snapper provider.Snapshotter, id, sandbox, ref string) {
	img := snapRepo + ":" + id

	err := snapper.Commit(ctx, ref, img)
	if err != nil {
		s.setSnap(id, func(r *snapshotRecord) {
			r.transition(snapFailed, "snapshot_capture_failed", fmt.Sprintf("docker commit of %s "+
				"failed: %v - check the engine has disk space (`docker system df`), then DELETE "+
				"this snapshot and try again", sandbox, err), s.now())
		})

		return
	}

	// A DELETE cannot remove a Creating snapshot, but a daemon shutdown can race the end of
	// the commit; if the record is gone the image is nobody's.
	if _, ok := s.setSnap(id, func(r *snapshotRecord) {
		r.transition(snapReady, "snapshot_ready", "", s.now())
	}); !ok {
		_ = snapper.RemoveImage(context.WithoutCancel(ctx), img)
		return
	}

	history.Append(history.Record{Kind: "event", Sandbox: sandbox, Event: "snapshot", Actor: "osb",
		Message: id + " -> " + img})
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	page, size, ok := pageParams(w, q.Get("page"), q.Get("pageSize"))
	if !ok {
		return
	}

	states := map[string]bool{}
	for _, st := range q["state"] {
		states[strings.ToLower(st)] = true
	}

	s.mu.Lock()

	var all []snapshotJSON

	for _, sr := range s.snaps {
		switch {
		case q.Get("sandboxId") != "" && sr.SandboxID != q.Get("sandboxId"):
		case q.Has("name") && sr.Name != q.Get("name"):
		case len(states) > 0 && !states[strings.ToLower(sr.State)]:
		default:
			all = append(all, sr.render())
		}
	}
	s.mu.Unlock()

	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}

		return all[i].ID < all[j].ID
	})

	items, pg := paginate(all, page, size)
	writeJSON(w, http.StatusOK, listSnapshotsJSON{Items: items, Pagination: pg})
}

func (s *Server) lookupSnap(w http.ResponseWriter, r *http.Request) (snapshotRecord, bool) {
	id := r.PathValue("sid")

	sr, ok := s.snap(id)
	if !snapIDPattern.MatchString(id) || !ok {
		writeErr(w, http.StatusNotFound, "SNAPSHOT::NOT_FOUND",
			fmt.Sprintf("no snapshot %q - ids look like snap-0123456789ab; GET /v1/snapshots lists them", id))

		return snapshotRecord{}, false
	}

	return sr, true
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) {
	if sr, ok := s.lookupSnap(w, r); ok {
		writeJSON(w, http.StatusOK, sr.render())
	}
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	sr, ok := s.lookupSnap(w, r)
	if !ok {
		return
	}

	if sr.State == snapCreating || sr.State == snapDeleting {
		writeErr(w, http.StatusConflict, "SNAPSHOT::NOT_READY",
			fmt.Sprintf("%s is %s; a snapshot still being created cannot be deleted - poll it until "+
				"it is Ready or Failed", sr.ID, sr.State))

		return
	}

	// Refused while something still runs on it, rather than forced: `docker rmi -f` would
	// untag the image and leave the sandbox running on a dangling one that no record names.
	if users := s.snapshotUsers(sr.ID); len(users) > 0 {
		writeErr(w, http.StatusConflict, "SNAPSHOT::DELETE_CONFLICT",
			fmt.Sprintf("%s is in use by %s - delete those first", sr.ID, strings.Join(users, ", ")))

		return
	}

	prev := sr.State
	s.setSnap(sr.ID, func(r *snapshotRecord) { r.transition(snapDeleting, "user_delete", "", s.now()) })

	if sr.State == snapReady {
		snapper, err := provider.SnapshotterFor(s.p)
		if err == nil {
			err = snapper.RemoveImage(r.Context(), sr.Image)
		}

		if err != nil {
			s.setSnap(sr.ID, func(r *snapshotRecord) { r.transition(prev, "delete_failed", err.Error(), s.now()) })

			status, code := http.StatusInternalServerError, "SANDBOX::DELETE_FAILED"
			if strings.Contains(strings.ToLower(err.Error()), "conflict") {
				status, code = http.StatusConflict, "SNAPSHOT::DELETE_CONFLICT"
			}

			writeErr(w, status, code, fmt.Sprintf("removing image %s: %v", sr.Image, err))

			return
		}
	}

	s.mu.Lock()
	delete(s.snaps, sr.ID)

	if err := os.Remove(filepath.Join(s.snapDir(), sr.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		logs.Default.Error(sr.SandboxID, "", "osb: could not delete the snapshot record: %v", err)
	}
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// snapshotUsers names the sandboxes and templates built on a snapshot.
func (s *Server) snapshotUsers(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string

	for _, r := range s.recs {
		if r.SnapshotID == id {
			out = append(out, "sandbox "+r.ID)
		}
	}

	for _, t := range s.tpls {
		if t.SnapshotID == id {
			out = append(out, "template "+t.ID)
		}
	}

	sort.Strings(out)

	return out
}

// recoverSnapshots settles snapshots a restart interrupted: a commit that finished before the
// daemon died left its image, and one that did not left nothing. Asked of docker rather than
// guessed, as upstream does.
func (s *Server) recoverSnapshots(ctx context.Context) {
	s.mu.Lock()

	var creating []string

	for id, sr := range s.snaps {
		if sr.State == snapCreating || sr.State == snapDeleting {
			creating = append(creating, id)
		}
	}
	s.mu.Unlock()

	if len(creating) == 0 {
		return
	}

	snapper, err := provider.SnapshotterFor(s.p)
	if err != nil {
		return
	}

	have, err := snapper.Images(ctx, snapRepo)
	if err != nil {
		logs.Default.Warn("", "", "osb: could not list snapshot images to recover: %v", err)
		return
	}

	for _, id := range creating {
		sr, _ := s.snap(id)

		if slices.Contains(have, sr.Image) {
			s.setSnap(id, func(r *snapshotRecord) {
				r.transition(snapReady, "snapshot_recovery_ready", "recovered after sbx serve restarted", s.now())
			})

			continue
		}

		s.setSnap(id, func(r *snapshotRecord) {
			r.transition(snapFailed, "snapshot_recovery_missing_image", "sbx serve restarted before "+
				"the commit finished and no image was saved; DELETE this snapshot and take another", s.now())
		})
	}
}

// readDir loads every <id>.json under dir whose id matches pattern into a fresh T. A file
// that cannot be parsed is reported and skipped - one corrupt record must not hide the rest.
func readDir[T any](dir string, pattern *regexp.Regexp) (map[string]*T, []error) {
	out := map[string]*T{}

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}

	if err != nil {
		return out, []error{err}
	}

	var errs []error

	for _, e := range entries {
		name := e.Name()
		id := strings.TrimSuffix(name, ".json")

		if filepath.Ext(name) != ".json" || !pattern.MatchString(id) {
			continue
		}

		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			errs = append(errs, err)
			continue
		}

		v := new(T)
		if err := json.Unmarshal(body, v); err != nil {
			errs = append(errs, fmt.Errorf("%s: not a record (%v) - move it aside", filepath.Join(dir, name), err))
			continue
		}

		out[id] = v
	}

	return out, errs
}

// pageParams reads page and pageSize, answering 400 itself when either is bad.
func pageParams(w http.ResponseWriter, p, ps string) (int, int, bool) {
	page, err := positive(p, 1)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", "page "+err.Error())
		return 0, 0, false
	}

	size, err := positive(ps, 20)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", "pageSize "+err.Error())
		return 0, 0, false
	}

	return page, size, true
}

func paginate[T any](all []T, page, size int) ([]T, paginationJSON) {
	total := len(all)
	pages := (total + size - 1) / size
	from := min((page-1)*size, total)
	to := min(from+size, total)

	items := slices.Clone(all[from:to])
	if items == nil {
		items = []T{}
	}

	return items, paginationJSON{Page: page, PageSize: size, TotalItems: total, TotalPages: pages,
		HasNextPage: page < pages}
}
