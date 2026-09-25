package osb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

const maxBody = 1 << 20

// ── records ──────────────────────────────────────────────────────────────────

// snapshot returns a copy of a record, so a handler can render it without holding the lock
// while it talks to docker.
func (s *Server) snapshot(id string) (record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.recs[id]
	if !ok {
		return record{}, false
	}

	c := *r
	c.Metadata = maps.Clone(r.Metadata)
	c.Extensions = maps.Clone(r.Extensions)
	c.Entrypoint = slices.Clone(r.Entrypoint)
	c.Ports = slices.Clone(r.Ports)
	c.OwnedVolumes = slices.Clone(r.OwnedVolumes)

	return c, true
}

// update applies f to a record and persists it. A failed write is logged, not returned: the
// in-memory record is still right for this process, and refusing the request would not make
// the disk writable.
func (s *Server) update(id string, f func(r *record)) (record, bool) {
	s.mu.Lock()

	r, ok := s.recs[id]
	if !ok {
		s.mu.Unlock()
		return record{}, false
	}

	f(r)

	if err := s.store.save(r); err != nil {
		logs.Default.Error(id, "", "osb: could not persist the sandbox record: %v", err)
	}
	s.mu.Unlock()

	return s.snapshot(id)
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (record, bool) {
	id := r.PathValue("id")

	rec, ok := s.snapshot(id)
	if !validID(id) || !ok {
		writeErr(w, http.StatusNotFound, "SANDBOX::NOT_FOUND",
			fmt.Sprintf("no sandbox %q - ids look like osb-0123456789ab; GET /v1/sandboxes lists them", id))

		return record{}, false
	}

	return rec, true
}

// unitsBySandbox asks the provider once for everything, so rendering a page of sandboxes is one
// docker call rather than one per sandbox.
func (s *Server) unitsBySandbox(ctx context.Context) (map[string][]provider.Unit, error) {
	units, err := s.p.List(ctx, "")
	if err != nil {
		return nil, err
	}

	out := map[string][]provider.Unit{}
	for _, u := range units {
		if validID(u.Sandbox) {
			out[u.Sandbox] = append(out[u.Sandbox], u)
		}
	}

	return out, nil
}

// liveStatus is the record's state corrected by what the container is actually doing.
//
// Only Running is corrected. Pending, Failed and Stopping are the API's own bookkeeping; Running
// is a claim about a container, so it is checked against one. Idle-frozen and idle-stopped are
// both reported as Running - the next request thaws or starts them without the caller doing
// anything, which is what Running promises - with a reason saying which.
func liveStatus(r record, units []provider.Unit) statusJSON {
	at := r.LastTransitionAt
	st := statusJSON{State: r.State, Reason: r.Reason, Message: r.Message, LastTransitionAt: &at}

	if r.State != stateRunning {
		return st
	}

	if r.PausedByAPI {
		return statusJSON{State: statePaused, Reason: "user_pause",
			Message:          "paused on request; memory and processes are kept. POST .../resume to continue",
			LastTransitionAt: &at}
	}

	if len(units) == 0 {
		return statusJSON{State: stateFailed, Reason: "container_missing",
			Message: "the container is gone - removed outside the API (sbx rm or docker rm?). " +
				"DELETE this sandbox to clear the record", LastTransitionAt: &at}
	}

	u := units[0]

	switch {
	case u.Paused:
		st.Reason, st.Message = "frozen", "idle, so frozen with memory kept; the next request thaws it"
	case !u.Running:
		st.Reason, st.Message = "stopped", "not running (idle sleep, or its entrypoint exited); "+
			"the next request starts it"
	}

	return st
}

func render(r record, units []provider.Unit, withImage bool) sandboxJSON {
	out := sandboxJSON{
		ID:         r.ID,
		Status:     liveStatus(r, units),
		Metadata:   r.Metadata,
		Extensions: r.Extensions,
		Platform:   r.Platform,
		Entrypoint: r.Entrypoint,
		ExpiresAt:  r.ExpiresAt,
		CreatedAt:  r.CreatedAt,
	}

	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}

	if out.Entrypoint == nil {
		out.Entrypoint = []string{}
	}

	if withImage {
		out.Image = &imageSpec{URI: r.Image}
	}

	return out
}

// ── handlers ─────────────────────────────────────────────────────────────────

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}

	units, err := s.unitsBySandbox(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SANDBOX::INTERNAL_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, render(rec, units[rec.ID], true))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	page, err := positive(q.Get("page"), 1)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", "page "+err.Error())
		return
	}

	size, err := positive(q.Get("pageSize"), 20)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", "pageSize "+err.Error())
		return
	}

	// metadata is itself a url-encoded query string: ?metadata=project%3DApollo%26team%3Dml.
	var meta url.Values
	if raw := q.Get("metadata"); raw != "" {
		if meta, err = url.ParseQuery(raw); err != nil {
			writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER",
				"metadata must be a url-encoded k=v&k2=v2 string: "+err.Error())

			return
		}
	}

	states := map[string]bool{}
	for _, st := range q["state"] {
		states[strings.ToLower(st)] = true
	}

	units, err := s.unitsBySandbox(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SANDBOX::INTERNAL_ERROR", err.Error())
		return
	}

	s.mu.Lock()
	ids := slices.Collect(maps.Keys(s.recs))
	s.mu.Unlock()

	var all []sandboxJSON

	for _, id := range ids {
		rec, ok := s.snapshot(id)
		if !ok {
			continue
		}

		sb := render(rec, units[id], true)

		if len(states) > 0 && !states[strings.ToLower(sb.Status.State)] {
			continue
		}

		if !metadataMatches(rec.Metadata, meta) {
			continue
		}

		all = append(all, sb)
	}

	// Oldest first, so a page does not shift under a caller walking them while sandboxes are
	// being created.
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}

		return all[i].ID < all[j].ID
	})

	total := len(all)
	pages := (total + size - 1) / size

	from := min((page-1)*size, total)
	to := min(from+size, total)

	items := all[from:to]
	if items == nil {
		items = []sandboxJSON{}
	}

	writeJSON(w, http.StatusOK, listJSON{
		Items: items,
		Pagination: paginationJSON{
			Page: page, PageSize: size, TotalItems: total, TotalPages: pages,
			HasNextPage: page < pages,
		},
	})
}

func metadataMatches(have map[string]string, want url.Values) bool {
	for k, vs := range want {
		for _, v := range vs {
			if have[k] != v {
				return false
			}
		}
	}

	return true
}

func positive(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}

	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q must be a whole number of at least 1", s)
	}

	return n, nil
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}

	if err := s.remove(r.Context(), rec.ID, "osb"); err != nil {
		writeErr(w, http.StatusInternalServerError, "SANDBOX::DELETE_FAILED", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// remove tears a sandbox down: container, data volume and record. actor is who asked, for the
// history: "osb" for a DELETE, "expiry" for the reaper.
func (s *Server) remove(ctx context.Context, id, actor string) error {
	s.mu.Lock()

	r, ok := s.recs[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}

	r.transition(stateStopping, "user_delete", "", s.now())

	if actor == "expiry" {
		r.Reason = "ttl_expiry"
	}

	cancel := s.provisioning[id]
	owned := slices.Clone(r.OwnedVolumes)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	s.rt.Hold(id, false)

	// The saved live policy goes with the sandbox, or a later sandbox reusing the name would
	// start enforcing a stranger's rules.
	if s.egress != nil {
		if err := s.egress.Forget(id); err != nil {
			logs.Default.Warn(id, service, "osb: could not forget the saved egress policy: %v", err)
		}
	}

	units, err := s.p.List(ctx, id)
	if err != nil {
		return fmt.Errorf("listing %s before removing it: %w", id, err)
	}

	if len(units) > 0 {
		// A frozen container is thawed first. Removing one is not reliably possible on every
		// runtime, and a remove that fails leaves a sandbox the caller believes is gone.
		if pa, ok := s.p.(provider.Pauser); ok {
			for _, u := range units {
				if u.Paused {
					_ = pa.Unpause(ctx, u.Ref)
				}
			}
		}

		if err := s.p.Remove(ctx, id); err != nil {
			s.update(id, func(r *record) {
				r.transition(stateFailed, "delete_failed", err.Error(), s.now())
			})

			return fmt.Errorf("removing %s: %w - `sbx rm %s` shows the same error with more detail", id, err, id)
		}
	}

	// After the container: docker will not remove a volume something still mounts.
	s.releaseClaims(ctx, id, owned)

	s.mu.Lock()
	delete(s.recs, id)
	delete(s.provisioning, id)

	if err := s.store.remove(id); err != nil {
		logs.Default.Error(id, "", "osb: could not delete the sandbox record: %v", err)
	}
	s.mu.Unlock()

	history.Append(history.Record{Kind: "event", Sandbox: id, Event: "removed", Actor: actor,
		Message: "removed through the OpenSandbox API"})

	return nil
}

func (s *Server) patchMetadata(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}

	var patch map[string]*string
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER",
			"the body must be a JSON object of string or null values: "+err.Error())

		return
	}

	for k, v := range patch {
		if err := checkMetadataKey(k); err != nil {
			writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_METADATA_LABEL", err.Error())
			return
		}

		if v != nil {
			if err := checkMetadataValue(k, *v); err != nil {
				writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_METADATA_LABEL", err.Error())
				return
			}
		}
	}

	// JSON Merge Patch (RFC 7396): a value sets, a null deletes, an absent key is left alone.
	updated, ok := s.update(rec.ID, func(r *record) {
		if r.Metadata == nil {
			r.Metadata = map[string]string{}
		}

		for k, v := range patch {
			if v == nil {
				delete(r.Metadata, k)
			} else {
				r.Metadata[k] = *v
			}
		}
	})
	if !ok {
		writeErr(w, http.StatusNotFound, "SANDBOX::NOT_FOUND", "the sandbox was removed during the patch")
		return
	}

	units, err := s.unitsBySandbox(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SANDBOX::INTERNAL_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, render(updated, units[updated.ID], true))
}

func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}

	var body renewRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil || body.ExpiresAt == nil {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER",
			`the body must be {"expiresAt": "<RFC 3339 time>"}`)

		return
	}

	next := body.ExpiresAt.UTC()
	now := s.now()

	if rec.ExpiresAt == nil {
		writeErr(w, http.StatusConflict, "SANDBOX::INVALID_EXPIRATION",
			fmt.Sprintf("%s was created without a timeout, so it never expires and there is "+
				"nothing to renew - DELETE it when you are done", rec.ID))

		return
	}

	if !next.After(now) {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_EXPIRATION",
			fmt.Sprintf("expiresAt %s is not in the future", next.Format(time.RFC3339)))

		return
	}

	if !next.After(*rec.ExpiresAt) {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_EXPIRATION",
			fmt.Sprintf("expiresAt %s must be later than the current expiry %s - renewing only "+
				"ever extends", next.Format(time.RFC3339), rec.ExpiresAt.Format(time.RFC3339)))

		return
	}

	if _, ok := s.update(rec.ID, func(r *record) { r.ExpiresAt = &next }); !ok {
		writeErr(w, http.StatusNotFound, "SANDBOX::NOT_FOUND", "the sandbox was removed during the renew")
		return
	}

	history.Append(history.Record{Kind: "event", Sandbox: rec.ID, Event: "renewed", Actor: "osb",
		Message: "expires " + next.Format(time.RFC3339)})

	writeJSON(w, http.StatusOK, renewResponse{ExpiresAt: next})
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}

	// Negotiated before anything is checked, so a backend that cannot pause says so the same
	// way whatever state the sandbox is in.
	if _, err := provider.PauserFor(s.p); err != nil {
		writeErr(w, http.StatusNotImplemented, "SANDBOX::API_NOT_SUPPORTED", err.Error())
		return
	}

	if rec.State != stateRunning || rec.PausedByAPI {
		state := rec.State
		if rec.PausedByAPI {
			state = statePaused
		}

		writeErr(w, http.StatusConflict, "SANDBOX::NOT_RUNNING",
			fmt.Sprintf("%s is %s; only a Running sandbox can be paused", rec.ID, state))

		return
	}

	if err := s.rt.Freeze(r.Context(), rec.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "SANDBOX::PAUSE_FAILED", err.Error())
		return
	}

	s.update(rec.ID, func(r *record) {
		r.PausedByAPI = true
		r.LastTransitionAt = s.now()
	})

	history.Append(history.Record{Kind: "event", Sandbox: rec.ID, Event: "paused", Actor: "osb"})

	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}

	if _, err := provider.PauserFor(s.p); err != nil {
		writeErr(w, http.StatusNotImplemented, "SANDBOX::API_NOT_SUPPORTED", err.Error())
		return
	}

	if !rec.PausedByAPI {
		writeErr(w, http.StatusConflict, "SANDBOX::NOT_PAUSED",
			fmt.Sprintf("%s is not paused; only a Paused sandbox can be resumed", rec.ID))

		return
	}

	if err := s.rt.Thaw(r.Context(), rec.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "SANDBOX::RESUME_FAILED", err.Error())
		return
	}

	s.update(rec.ID, func(r *record) {
		r.PausedByAPI = false
		r.LastTransitionAt = s.now()
	})

	history.Append(history.Record{Kind: "event", Sandbox: rec.ID, Event: "resumed", Actor: "osb"})

	w.WriteHeader(http.StatusAccepted)
}

// endpoint resolves a port inside the sandbox to an address the caller can reach from here.
//
// execd's port is the daemon's own wake port for the sandbox - so the caller's first request
// thaws or starts it, and it never sees a refused connection. A port listed in
// extensions["sbx.ports"] got its own wake port at create. Any other port goes through execd's
// /proxy/{port}, because OpenSandbox lets a client ask for a port nobody declared and a slot's
// ports are fixed when its container is created.
func (s *Server) endpoint(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}

	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PORT",
			fmt.Sprintf("port %q must be a number from 1 to 65535", r.PathValue("port")))

		return
	}

	q := r.URL.Query()
	if v := q.Get("use_server_proxy"); v == "true" {
		notYet(w, "server-proxied endpoints", "v0.11.0")
		return
	}

	if q.Has("expires") {
		notYet(w, "signed endpoints", "v0.11.0")
		return
	}

	// 18080 is where upstream's egress sidecar listens. sbx's filter is not in the sandbox, so
	// the endpoint is this API's own sidecar-shaped route - unless the caller declared 18080 as
	// a port of their own.
	if port == egressPort && !slices.Contains(rec.Ports, egressPort) {
		s.egressEndpoint(w, r, rec)
		return
	}

	units, err := s.unitsBySandbox(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SANDBOX::INTERNAL_ERROR", err.Error())
		return
	}

	us := units[rec.ID]
	if len(us) == 0 || len(us[0].Client) < len(rec.Ports) {
		writeErr(w, http.StatusNotFound, "SANDBOX::NOT_READY",
			fmt.Sprintf("%s has no container yet (state %s) - wait for Running", rec.ID, rec.State))

		return
	}

	addr := func(i int) string { return us[0].Client[i].String() }
	auth := map[string]string{tokenHeader: rec.Token}

	for i, p := range rec.Ports {
		if p != port {
			continue
		}

		if p == execdPort {
			writeJSON(w, http.StatusOK, endpointJSON{Endpoint: addr(i), Headers: auth})
		} else {
			writeJSON(w, http.StatusOK, endpointJSON{Endpoint: addr(i)})
		}

		return
	}

	writeJSON(w, http.StatusOK, endpointJSON{
		Endpoint: addr(0) + "/proxy/" + strconv.Itoa(port),
		Headers:  auth,
	})
}

// diagnostics serves the stable Diagnostics API: a JSON descriptor with the text inline. With
// no ?scope it serves the deprecated plain-text form the spec still allows, which is what an
// older client or a person with curl gets.
func (s *Server) diagnostics(kind string) http.HandlerFunc {
	scopes := map[string][]string{
		"logs":   {"container", "lifecycle", "all"},
		"events": {"lifecycle", "runtime", "all"},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		rec, ok := s.lookup(w, r)
		if !ok {
			return
		}

		scope, legacy := r.URL.Query().Get("scope"), !r.URL.Query().Has("scope")
		if legacy {
			scope = "all"
		}

		if !slices.Contains(scopes[kind], scope) {
			writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER",
				fmt.Sprintf("scope %q is not one sbx serves for %s; use one of %s",
					scope, kind, strings.Join(scopes[kind], ", ")))

			return
		}

		var (
			buf      bytes.Buffer
			warnings []string
		)

		if kind == "logs" && scope != "lifecycle" {
			if w := s.containerLogs(r.Context(), rec.ID, &buf); w != "" {
				warnings = append(warnings, w)
			}
		}

		if kind == "events" || scope != "container" {
			s.lifecycleText(rec, &buf)
		}

		const limit = 1 << 20

		text, truncated := buf.String(), false
		if len(text) > limit {
			text, truncated = text[len(text)-limit:], true
		}

		if legacy {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, text)

			return
		}

		writeJSON(w, http.StatusOK, diagnosticJSON{
			SandboxID: rec.ID, Kind: kind, Scope: scope, Delivery: "inline",
			ContentType: "text/plain; charset=utf-8", Content: text, ContentLength: len(text),
			Truncated: truncated, Warnings: warnings,
		})
	}
}

func (s *Server) containerLogs(ctx context.Context, id string, w io.Writer) string {
	units, err := s.p.List(ctx, id)
	if err != nil {
		return "could not list the container: " + err.Error()
	}

	if len(units) == 0 {
		return "no container exists for this sandbox, so there are no container logs"
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := s.p.Logs(ctx, units[0].Ref, 1000, false, w); err != nil {
		return "reading container logs failed: " + err.Error()
	}

	return ""
}

func (s *Server) lifecycleText(rec record, w io.Writer) {
	_, _ = fmt.Fprintf(w, "%s created %s image %s\n", rec.CreatedAt.Format(time.RFC3339), rec.ID, rec.Image)
	_, _ = fmt.Fprintf(w, "%s state %s %s %s\n", rec.LastTransitionAt.Format(time.RFC3339), rec.State,
		rec.Reason, rec.Message)

	events, err := history.Read(history.Filter{Sandbox: rec.ID, Limit: 500})
	if err != nil {
		return
	}

	for _, e := range events {
		actor := e.Actor
		if actor == "" {
			actor = "daemon"
		}

		_, _ = fmt.Fprintf(w, "%s %s %s %s %s\n", e.Time.UTC().Format(time.RFC3339), actor,
			cmp(e.Event, e.Kind), e.Service, e.Message)
	}
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	var ev metricsEvent
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&ev); err != nil {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", "the body must be a MetricsEvent: "+err.Error())
		return
	}

	switch {
	case ev.EventType != "sandbox.create":
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER",
			fmt.Sprintf("eventType %q is not one this server knows; the only one is sandbox.create", ev.EventType))

		return
	case ev.CreateDurationMs == nil || *ev.CreateDurationMs < 0:
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", "createDurationMs is required and must be >= 0")
		return
	case ev.Success == nil:
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", "success is required")
		return
	}

	history.Append(history.Record{
		Kind: "event", Sandbox: ev.SandboxID, Event: "sdk.create", Actor: "sdk",
		DurationMs: *ev.CreateDurationMs, Failed: !*ev.Success,
		Message: strings.TrimSpace(ev.Image + " " + r.UserAgent()),
	})

	w.WriteHeader(http.StatusNoContent)
}

var errGone = errors.New("the sandbox was deleted while it was being created")
