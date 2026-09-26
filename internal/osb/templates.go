package osb

// Templates: POST|GET /templates, GET|DELETE /templates/{id}, and templateId on create.
//
// Upstream's templates are fast-sandbox golden images - a microVM build published to S3 - and
// its docker runtime answers 501. sbx has no microVM and no S3, but it has what a template is
// FOR: a fixed workload somebody can start many sandboxes from by id. So here a template is an
// image, or a snapshot, plus the create options that go with it (entrypoint, cpu, memory).
// "Building" is making sure the image is present and runs on this machine's architecture; a
// template backed by a snapshot is ready as soon as the snapshot is.
//
// What does not translate is refused rather than stored and ignored: readiness (a build-time
// probe of a VM sbx does not boot) and resourceLimits.disk (docker cannot cap a container's
// root filesystem everywhere). `publish` and `format` are recorded and echoed because the spec
// requires them in the response, and they change nothing - the image stays in the local
// engine, and manifestRef names it there.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

const (
	tplPending   = "Pending"
	tplBuilding  = "Building"
	tplSucceeded = "Succeeded"
	tplFailed    = "Failed"
)

var tplIDPattern = regexp.MustCompile(`^tpl-[0-9a-f]{12}$`)

type templateRecord struct {
	ID string `json:"templateId"`

	// Image is what the caller named: an image reference, or a snapshot id.
	Image string `json:"image"`

	// SnapshotID is set when Image named a snapshot; RunImage is what sandboxes run either way.
	SnapshotID string `json:"snapshotId,omitempty"`
	RunImage   string `json:"runImage"`

	ResourceLimits map[string]string `json:"resourceLimits,omitempty"`
	Entrypoint     []string          `json:"entrypoint,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Publish        string            `json:"publish"`
	Format         string            `json:"format"`

	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type templateStatusJSON struct {
	Phase       string `json:"phase"`
	ManifestRef string `json:"manifestRef,omitempty"`
	Message     string `json:"message,omitempty"`
}

type templateJSON struct {
	TemplateID     string             `json:"templateId"`
	Image          string             `json:"image"`
	ResourceLimits map[string]string  `json:"resourceLimits,omitempty"`
	Entrypoint     []string           `json:"entrypoint,omitempty"`
	Metadata       map[string]string  `json:"metadata,omitempty"`
	Publish        string             `json:"publish"`
	Format         string             `json:"format"`
	Status         templateStatusJSON `json:"status"`
	CreatedAt      time.Time          `json:"createdAt"`
	UpdatedAt      time.Time          `json:"updatedAt"`
}

type listTemplatesJSON struct {
	Items      []templateJSON `json:"items"`
	Pagination paginationJSON `json:"pagination"`
}

type createTemplateRequest struct {
	Image          string            `json:"image"`
	ResourceLimits map[string]string `json:"resourceLimits"`
	Entrypoint     []string          `json:"entrypoint"`
	Metadata       map[string]string `json:"metadata"`
	Readiness      json.RawMessage   `json:"readiness"`
	Publish        string            `json:"publish"`
	Format         string            `json:"format"`
}

func (t templateRecord) render() templateJSON {
	st := templateStatusJSON{Phase: t.Phase, Message: t.Message}
	if t.Phase == tplSucceeded {
		// The spec's manifestRef is an S3 manifest. Here the artifact is an image in the local
		// engine, and this names it in a form nobody will mistake for an S3 URL.
		st.ManifestRef = "docker-image://" + t.RunImage
	}

	return templateJSON{TemplateID: t.ID, Image: t.Image, ResourceLimits: t.ResourceLimits,
		Entrypoint: t.Entrypoint, Metadata: t.Metadata, Publish: t.Publish, Format: t.Format,
		Status: st, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func (s *Server) tplDir() string { return filepath.Join(s.store.dir, "templates") }

func newTplID() string { return "tpl-" + newID()[len("osb-"):] }

func (s *Server) setTpl(id string, f func(t *templateRecord)) (templateRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tpls[id]
	if !ok {
		return templateRecord{}, false
	}

	f(t)
	t.UpdatedAt = s.now().UTC()

	if err := writeAtomic(s.tplDir(), t.ID, t); err != nil {
		logs.Default.Error("", "", "osb: could not persist template %s: %v", t.ID, err)
	}

	return *t, true
}

func (s *Server) tpl(id string) (templateRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tpls[id]
	if !ok {
		return templateRecord{}, false
	}

	return *t, true
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	bad := func(msg string, args ...any) {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", fmt.Sprintf(msg, args...))
	}

	var req createTemplateRequest

	raw, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		bad("the body is not a CreateFsbTemplateRequest: %v", err)
		return
	}

	req.Image = strings.TrimSpace(req.Image)

	switch {
	case req.Image == "":
		bad(`image is required: an image reference such as "python:3.11-slim", or a snapshot id`)
		return
	case present(req.Readiness):
		writeErr(w, http.StatusNotImplemented, "SANDBOX::API_NOT_SUPPORTED", "readiness is a probe "+
			"run while a fast-sandbox microVM image is built; sbx builds no VM image, so there is "+
			"nothing for it to gate. Omit it - a sandbox created from the template is still only "+
			"Running once execd answers")

		return
	case req.Format != "" && req.Format != "native" && req.Format != "overlaybd":
		bad("format %q must be native or overlaybd", req.Format)
		return
	case len(req.Entrypoint) > 0 && strings.TrimSpace(req.Entrypoint[0]) == "":
		bad("entrypoint[0] is empty; it must name the program to run")
		return
	}

	if err := checkMetadata(req.Metadata); err != nil {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_METADATA_LABEL", err.Error())
		return
	}

	for k, v := range req.ResourceLimits {
		var err error

		switch k {
		case "cpu":
			_, err = parseCPU(v)
		case "memory":
			_, err = parseMemory(v)
		default:
			err = fmt.Errorf("resourceLimits.%s is not a limit sbx can apply to a template - cpu "+
				"and memory are (disk would need a root filesystem quota docker does not offer "+
				"on every storage driver)", k)
		}

		if err != nil {
			bad("%v", err)
			return
		}
	}

	if req.Format == "" {
		req.Format = "overlaybd"
	}

	now := s.now().UTC()
	t := &templateRecord{
		ID: newTplID(), Image: req.Image, RunImage: req.Image, ResourceLimits: req.ResourceLimits,
		Entrypoint: req.Entrypoint, Metadata: req.Metadata, Publish: req.Publish, Format: req.Format,
		Phase: tplPending, CreatedAt: now, UpdatedAt: now,
	}

	if snapIDPattern.MatchString(req.Image) {
		sr, ok := s.snap(req.Image)
		if !ok {
			writeErr(w, http.StatusNotFound, "SNAPSHOT::NOT_FOUND", fmt.Sprintf("image %q names a "+
				"snapshot that does not exist - GET /v1/snapshots lists them", req.Image))

			return
		}

		t.SnapshotID, t.RunImage = sr.ID, sr.Image
	}

	s.mu.Lock()
	s.tpls[t.ID] = t

	if err := writeAtomic(s.tplDir(), t.ID, t); err != nil {
		delete(s.tpls, t.ID)
		s.mu.Unlock()

		writeErr(w, http.StatusInternalServerError, "SANDBOX::INTERNAL_ERROR",
			fmt.Sprintf("could not record the template under %s: %v", s.tplDir(), err))

		return
	}

	resp := t.render()
	s.mu.Unlock()

	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		s.buildTemplate(s.base, resp.TemplateID)
	}()

	writeJSON(w, http.StatusCreated, resp)
}

// buildTemplate makes the template usable: its snapshot Ready, or its image pulled and
// inspectable here. A template that cannot start a sandbox must say so now, not at the first
// create.
func (s *Server) buildTemplate(ctx context.Context, id string) {
	t, ok := s.setTpl(id, func(t *templateRecord) { t.Phase = tplBuilding })
	if !ok {
		return
	}

	fail := func(msg string) {
		s.setTpl(id, func(t *templateRecord) { t.Phase, t.Message = tplFailed, msg })
	}

	if t.SnapshotID != "" {
		deadline := s.now().Add(s.readyTimeout)

		for {
			sr, ok := s.snap(t.SnapshotID)

			switch {
			case !ok:
				fail("snapshot " + t.SnapshotID + " was deleted before the template was ready")
				return
			case sr.State == snapReady:
				s.setTpl(id, func(t *templateRecord) { t.Phase = tplSucceeded })
				return
			case sr.State != snapCreating || s.now().After(deadline):
				fail(fmt.Sprintf("snapshot %s is %s (%s)", sr.ID, sr.State, sr.Message))
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}

	if pu, ok := s.p.(provider.Puller); ok {
		if err := pu.Pull(ctx, t.RunImage); err != nil {
			fail(fmt.Sprintf("pulling %s: %v - check the name and that this machine can reach "+
				"its registry (`docker pull %s`)", t.RunImage, err, t.RunImage))

			return
		}
	}

	insp, err := s.inspector()
	if err != nil {
		fail(err.Error())
		return
	}

	info, err := insp.ImageInfo(ctx, t.RunImage)
	if err != nil {
		fail(err.Error())
		return
	}

	if info.OS != "" && info.OS != "linux" {
		fail(fmt.Sprintf("%s is a %s image; sbx runs linux containers only", t.RunImage, info.OS))
		return
	}

	s.setTpl(id, func(t *templateRecord) { t.Phase = tplSucceeded })
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	page, size, ok := pageParams(w, q.Get("page"), q.Get("pageSize"))
	if !ok {
		return
	}

	var meta url.Values
	if raw := q.Get("metadata"); raw != "" {
		var err error
		if meta, err = url.ParseQuery(raw); err != nil {
			writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER",
				"metadata must be a url-encoded k=v&k2=v2 string: "+err.Error())

			return
		}
	}

	s.mu.Lock()

	var all []templateJSON

	for _, t := range s.tpls {
		if metadataMatches(t.Metadata, meta) {
			all = append(all, t.render())
		}
	}
	s.mu.Unlock()

	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}

		return all[i].TemplateID < all[j].TemplateID
	})

	items, pg := paginate(all, page, size)
	writeJSON(w, http.StatusOK, listTemplatesJSON{Items: items, Pagination: pg})
}

func (s *Server) lookupTpl(w http.ResponseWriter, r *http.Request) (templateRecord, bool) {
	id := r.PathValue("tid")

	t, ok := s.tpl(id)
	if !tplIDPattern.MatchString(id) || !ok {
		writeErr(w, http.StatusNotFound, "FSB::TEMPLATE_NOT_FOUND",
			fmt.Sprintf("no template %q - ids look like tpl-0123456789ab; GET /v1/templates lists them", id))

		return templateRecord{}, false
	}

	return t, true
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request) {
	if t, ok := s.lookupTpl(w, r); ok {
		writeJSON(w, http.StatusOK, t.render())
	}
}

// deleteTemplate removes the record only. Sandboxes made from it keep running - they hold
// their own image reference - and the image or snapshot behind it is somebody else's to
// delete: a public image may be in use by anything, and a snapshot has its own DELETE.
func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := s.lookupTpl(w, r)
	if !ok {
		return
	}

	s.mu.Lock()
	delete(s.tpls, t.ID)

	if err := os.Remove(filepath.Join(s.tplDir(), t.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		logs.Default.Error("", "", "osb: could not delete the template record: %v", err)
	}
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// recoverTemplates restarts builds a restart interrupted.
func (s *Server) recoverTemplates() {
	s.mu.Lock()

	var ids []string

	for id, t := range s.tpls {
		if t.Phase == tplPending || t.Phase == tplBuilding {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()

	for _, id := range ids {
		s.wg.Add(1)

		go func() {
			defer s.wg.Done()
			s.buildTemplate(s.base, id)
		}()
	}
}

// templatePlanFields are the create fields template mode rejects: the spec fixes the workload
// shape to the template's.
func templateRejects(req createRequest) string {
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"image", req.Image != nil},
		{"snapshotId", req.SnapshotID != ""},
		{"entrypoint", len(req.Entrypoint) > 0},
		{"env", len(req.Env) > 0},
		{"resourceLimits", len(req.ResourceLimits) > 0},
		{"resourceRequests", len(req.ResourceRequests) > 0},
		{"volumes", len(req.Volumes) > 0},
		{"platform", req.Platform != nil},
		{"credentialProxy", present(req.CredentialProxy)},
		{"secureAccess", req.SecureAccess},
		{"lifecycle", present(req.Lifecycle)},
	} {
		if f.set {
			return f.name
		}
	}

	return ""
}
