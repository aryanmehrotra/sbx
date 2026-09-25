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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// service is the one service every API sandbox has. One, because an OpenSandbox sandbox is one
// container; named, because every sbx command addresses a service by name.
const service = "sandbox"

// plan is a validated create request: everything provisioning needs, and nothing it would have
// to check again.
type plan struct {
	rec    record
	env    map[string]string // held in memory only: env is where people put secrets
	cpu    string
	memory string
	gpus   string
	onIdle string

	// egressPolicy is the networkPolicy the sandbox starts with; nil is no filter at all.
	egressPolicy *egress.Policy
}

// create validates synchronously and provisions in the background: 202 with Pending, then
// Running once execd answers through the address the caller will be given.
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest

	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", "the body is not a CreateSandboxRequest: "+err.Error())
		return
	}

	pl, status, code, msg := s.validate(req)
	if status != 0 {
		writeErr(w, status, code, msg)
		return
	}

	if s.fromPool(w, r, req, pl) {
		return
	}

	accepted := pl.rec // &pl.rec is the live record from the next line on

	s.mu.Lock()
	s.recs[accepted.ID] = &pl.rec
	s.mu.Unlock()

	if err := s.persist(accepted.ID); err != nil {
		s.mu.Lock()
		delete(s.recs, accepted.ID)
		s.mu.Unlock()

		writeErr(w, http.StatusInternalServerError, "SANDBOX::INTERNAL_ERROR",
			fmt.Sprintf("could not record the sandbox under %s: %v", s.store.dir, err))

		return
	}

	s.trace.begin(accepted.ID)

	ctx, cancel := context.WithCancel(s.base)

	s.mu.Lock()
	s.provisioning[accepted.ID] = cancel
	s.mu.Unlock()

	s.history(history.Record{Kind: "event", Sandbox: accepted.ID, Event: "created", Actor: "osb",
		Message: "image " + accepted.Image})

	done := make(chan struct{})

	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		defer cancel()
		defer close(done)

		s.provision(ctx, pl)
	}()

	s.answerCreate(w, r, accepted, done)
}

// answerCreate holds the response until provisioning finishes or createWait runs out, then
// answers with whatever state the sandbox is in. Always 202: the spec's create status, and a
// client that reads the body sees Running or Pending either way.
func (s *Server) answerCreate(w http.ResponseWriter, r *http.Request, accepted record, done <-chan struct{}) {
	id := accepted.ID

	if s.createWait > 0 {
		t := time.NewTimer(s.createWait)

		select {
		case <-done:
		case <-t.C:
		case <-r.Context().Done():
		}

		t.Stop()
	}

	// A DELETE during the wait removed the record; the sandbox was still accepted, so the
	// caller gets what it was accepted as, and its next GET says it is gone.
	rec, ok := s.snapshot(id)
	if !ok {
		rec = accepted
	}
	resp := render(rec, nil, false)

	// render checks Running against the container, and this path has none to hand: it has
	// just watched execd answer through the wake port, which is a stronger claim than a list.
	if rec.State == stateRunning {
		at := rec.LastTransitionAt
		resp.Status = statusJSON{State: stateRunning, LastTransitionAt: &at}
	}

	s.trace.mark(id, "create answered "+resp.Status.State)

	w.Header().Set("Location", "/v1/sandboxes/"+id)
	writeJSON(w, http.StatusAccepted, resp)
}

// validate turns a request into a plan, or into the refusal the spec prescribes. Everything
// that can be known without docker is known here, so a bad request is a 400 now rather than a
// Failed sandbox a poll later.
func (s *Server) validate(req createRequest) (plan, int, string, string) {
	bad := func(msg string, args ...any) (plan, int, string, string) {
		return plan{}, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", fmt.Sprintf(msg, args...)
	}

	later := func(what, release string) (plan, int, string, string) {
		return plan{}, http.StatusNotImplemented, "SANDBOX::API_NOT_SUPPORTED",
			fmt.Sprintf("%s is not supported by this sbx yet; it arrives in sbx %s", what, release)
	}

	if _, err := provider.InjectorFor(s.p); err != nil {
		return plan{}, http.StatusNotImplemented, "SANDBOX::API_NOT_SUPPORTED", err.Error()
	}

	switch {
	case req.SnapshotID != "":
		return later("creating from a snapshot (snapshotId)", "v0.10.0")
	case req.TemplateID != "":
		return later("creating from a template (templateId)", "v0.10.0")
	case req.Extensions["poolRef"] != "":
		return later("server-side pools (extensions.poolRef)", "v0.11.0")
	case req.Image == nil || strings.TrimSpace(req.Image.URI) == "":
		return bad("image.uri is required - for example {\"image\": {\"uri\": \"python:3.11-slim\"}}")
	case len(req.Image.Auth) > 0 && string(req.Image.Auth) != "null":
		return later("registry credentials in image.auth (log in with `docker login` on this "+
			"machine and omit them)", "v0.11.0")
	case req.SecureAccess:
		return later("secureAccess", "v0.11.0")
	case present(req.CredentialProxy):
		return later("credentialProxy", "v0.11.0")
	case present(req.Lifecycle):
		return later("lifecycle hooks", "v0.11.0")
	case req.Extensions["access.renew.extend.seconds"] != "":
		return later("renew-on-access (extensions[\"access.renew.extend.seconds\"])", "v0.11.0")
	}

	policy, err := createPolicy(req.NetworkPolicy)
	if err != nil {
		var refused *egress.Error
		if errors.As(err, &refused) {
			return plan{}, http.StatusBadRequest, "SANDBOX::" + refused.Code, refused.Message
		}

		return bad("networkPolicy: %v", err)
	}

	if policy != nil && s.egress == nil {
		return later("networkPolicy (this sbx serve has no egress control)", "a daemon with egress control")
	}

	for _, raw := range req.Volumes {
		var v map[string]json.RawMessage
		_ = json.Unmarshal(raw, &v)

		if _, ok := v["ossfs"]; ok {
			return plan{}, http.StatusBadRequest, "VOLUME::INVALID_BACKEND",
				"ossfs volumes mount Alibaba OSS buckets, which sbx does not support and does " +
					"not plan to - use a host or pvc volume"
		}

		return later("volumes", "v0.10.0")
	}

	if req.Timeout != nil && *req.Timeout < 60 {
		return bad("timeout %d is below the minimum of 60 seconds; omit it (or send null) for a "+
			"sandbox that never expires", *req.Timeout)
	}

	if len(req.Entrypoint) > 0 && strings.TrimSpace(req.Entrypoint[0]) == "" {
		return plan{}, http.StatusBadRequest, "SANDBOX::INVALID_ENTRYPOINT",
			"entrypoint[0] is empty; it must name the program to run"
	}

	if err := checkMetadata(req.Metadata); err != nil {
		return plan{}, http.StatusBadRequest, "SANDBOX::INVALID_METADATA_LABEL", err.Error()
	}

	for k := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return bad("env key %q is not a valid variable name", k)
		}

		if k == tokenEnv {
			return bad("env %s is reserved: it carries execd's access token, which sbx mints", tokenEnv)
		}
	}

	if p := req.Platform; p != nil {
		if p.OS != "linux" {
			return bad("platform.os %q: sbx runs linux containers only", p.OS)
		}

		if p.Arch != "amd64" && p.Arch != "arm64" {
			return bad("platform.arch %q must be amd64 or arm64", p.Arch)
		}
	}

	pl := plan{onIdle: spec.OnIdleFreeze, egressPolicy: policy}

	for k, v := range req.ResourceLimits {
		switch k {
		case "cpu":
			c, err := parseCPU(v)
			if err != nil {
				return bad("%v", err)
			}

			pl.cpu = strconv.FormatFloat(c, 'f', -1, 64)
		case "memory":
			m, err := parseMemory(v)
			if err != nil {
				return bad("%v", err)
			}

			pl.memory = strconv.FormatUint(m, 10)
		case "gpu":
			pl.gpus = strings.TrimSpace(v)
		default:
			// Refused rather than ignored: a limit that is silently dropped is a sandbox running
			// without the ceiling its caller believes it has.
			return bad("resourceLimits.%s is not a limit sbx can apply - cpu, memory and gpu are", k)
		}
	}

	extra, err := parsePorts(req.Extensions["sbx.ports"])
	if err != nil {
		return bad("%v", err)
	}

	switch req.Extensions["sbx.pool"] {
	case "", "off":
	default:
		return bad("extensions[\"sbx.pool\"] %q must be \"off\" (never serve this create from a warm "+
			"pool) or absent", req.Extensions["sbx.pool"])
	}

	switch req.Extensions["sbx.idle"] {
	case "", "freeze":
	case "sleep":
		pl.onIdle = ""
	default:
		return bad("extensions[\"sbx.idle\"] %q must be \"freeze\" (the default: memory kept) "+
			"or \"sleep\" (stopped to 0 B; background processes do not survive)", req.Extensions["sbx.idle"])
	}

	// A provider that cannot pause still gets a sandbox: idle then stops it. That is the one
	// place this degrades rather than refuses, because nothing was ASKED for - freeze is sbx's
	// default here, not the caller's request.
	if _, err := provider.PauserFor(s.p); err != nil {
		pl.onIdle = ""
	}

	now := s.now().UTC()

	pl.env = req.Env
	pl.rec = record{
		ID:               s.newID(),
		Image:            strings.TrimSpace(req.Image.URI),
		Entrypoint:       slices.Clone(req.Entrypoint),
		Metadata:         maps.Clone(req.Metadata),
		Extensions:       maps.Clone(req.Extensions),
		Platform:         req.Platform,
		Ports:            append([]int{execdPort}, extra...),
		Token:            newToken(),
		CreatedAt:        now,
		State:            statePending,
		Reason:           "provisioning",
		Message:          "pulling the image and starting the sandbox",
		LastTransitionAt: now,
	}

	if req.Timeout != nil {
		exp := now.Add(time.Duration(*req.Timeout) * time.Second)
		pl.rec.ExpiresAt = &exp
	}

	return pl, 0, "", ""
}

func present(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t != "" && t != "null" && t != "{}"
}

// provision does the slow part: pull, place execd, create, wait for /ping.
func (s *Server) provision(ctx context.Context, pl plan) {
	id := pl.rec.ID

	fail := func(reason, msg string) {
		s.update(id, func(r *record) { r.transition(stateFailed, reason, msg, s.now()) })
		logs.Default.Error(id, service, "osb: %s: %s", reason, msg)
	}

	inj, err := provider.InjectorFor(s.p)
	if err != nil {
		fail("unsupported", err.Error())
		return
	}

	// Pulled only when absent, as `docker run` and upstream's docker runtime both do. Pulling a
	// tag that is already here still asks the registry for its manifest - 2.8-3.2 s per create
	// on colima, measured, most of a cold create - and learns nothing unless the tag moved,
	// which is what an explicit `docker pull` on this machine is for.
	info, err := s.images.info(ctx, pl.rec.Image, inj.ImageInfo)
	if err != nil {
		if pu, ok := s.p.(provider.Puller); ok {
			if err := pu.Pull(ctx, pl.rec.Image); err != nil {
				fail("image_pull_failed", fmt.Sprintf("pulling %s: %v - check the name and that this "+
					"machine can reach its registry (`docker pull %s`)", pl.rec.Image, err, pl.rec.Image))

				return
			}

			s.trace.mark(id, "image pulled")

			s.images.forget(pl.rec.Image)

			info, err = s.images.info(ctx, pl.rec.Image, inj.ImageInfo)
		}

		if err != nil {
			fail("image_inspect_failed", err.Error())
			return
		}
	}

	s.trace.mark(id, "image inspected")

	arch := normalizeArch(info.Arch)

	if p := pl.rec.Platform; p != nil && (info.OS != p.OS || arch != p.Arch) {
		fail("platform_mismatch", fmt.Sprintf("image %s is %s/%s, not the requested %s/%s - pull "+
			"the variant you want (`docker pull --platform %s/%s %s`) or drop platform",
			pl.rec.Image, info.OS, arch, p.OS, p.Arch, p.OS, p.Arch, pl.rec.Image))

		return
	}

	entry := pl.rec.Entrypoint
	if len(entry) == 0 {
		entry = append(slices.Clone(info.Entrypoint), info.Cmd...)
		if len(entry) == 0 {
			fail("invalid_entrypoint", fmt.Sprintf("no entrypoint was given and %s declares no "+
				"ENTRYPOINT or CMD - pass one, for example [\"tail\", \"-f\", \"/dev/null\"]", pl.rec.Image))

			return
		}

		s.update(id, func(r *record) { r.Entrypoint = entry })
	}

	vol, err := s.placeExecd(ctx, inj, arch, pl.rec.Image)
	if err != nil {
		fail("execd_unavailable", err.Error())
		return
	}

	s.trace.mark(id, "execd placed")

	env := maps.Clone(pl.env)
	if env == nil {
		env = map[string]string{}
	}

	env[tokenEnv] = pl.rec.Token

	svc := spec.Service{
		Image: pl.rec.Image,
		Ports: pl.rec.Ports,
		Env:   env,
		Entrypoint: append([]string{execdMount + "/" + execdBinary, "execd",
			"--addr", ":" + strconv.Itoa(execdPort), "--"}, entry...),
		ReadOnlyVolumes: map[string]string{vol: execdMount},

		// Declared so the daemon's wake path can verify execd is serving rather than sleeping
		// two seconds and hoping - the fallback it takes for a service with no health check.
		// httpcheck is sbx's own, so it works in an image with no curl or wget; it does need
		// /bin/sh, because docker runs a health command through one.
		Health:         execdMount + "/" + execdBinary + " httpcheck http://127.0.0.1:" + strconv.Itoa(execdPort) + "/ping",
		HealthInterval: "5s",
		CPU:            pl.cpu,
		Memory:         pl.memory,
		GPUs:           pl.gpus,
		OnIdle:         pl.onIdle,
		EgressPolicy:   pl.egressPolicy,
	}

	if err := svc.Validate(service); err != nil {
		fail("invalid_request", err.Error())
		return
	}

	// Asked before anything is created: whether this machine can run the filter is a property
	// of the host, and finding out at `docker run` would leave a half-made sandbox behind.
	if svc.Filtered() {
		if pf, ok := s.p.(provider.EgressPreflighter); ok {
			if err := pf.EgressPreflight(ctx, id); err != nil {
				fail("egress_unavailable", err.Error())
				return
			}
		}
	}

	if err := s.createContainer(ctx, id, svc); err != nil {
		if errors.Is(err, errGone) {
			return
		}

		fail("create_failed", err.Error())

		return
	}

	s.trace.mark(id, "container created")

	// The server's context, not this create's: the daemon derives the new sandbox's listeners
	// from the context it is refreshed with, and this create's is cancelled the moment it
	// finishes - which closed the wake port just as the sandbox became Running.
	s.rt.Refresh(s.base)
	s.trace.mark(id, "daemon refreshed")

	s.waitReady(ctx, id)
}

// createContainer allocates the slot and creates the container under the machine's slot lock,
// then checks the sandbox was not deleted while that was happening.
func (s *Server) createContainer(ctx context.Context, id string, svc spec.Service) error {
	if picker, ok := s.p.(provider.SlotPicker); ok {
		return s.createPicked(ctx, id, svc, picker)
	}

	// Released as soon as the container exists (it then holds the slot for everyone to see),
	// and on every other path by the defer - once either way.
	var once sync.Once

	unlock := s.lockSlots()
	release := func() { once.Do(unlock) }

	defer release()

	s.trace.mark(id, "slot lock held")

	slot, err := s.p.AllocSlot(ctx, id)
	if err != nil {
		return err
	}

	s.trace.mark(id, "slot allocated")

	eps := s.p.Endpoints(id, service, slot, 0, svc.Ports)

	if err := s.p.Create(ctx, id, slot, 0, service, svc, eps, "", provider.IsolationContainer); err != nil {
		return err
	}

	release()

	// A DELETE that arrived while docker was creating found no container and removed only the
	// record. Without this the container would outlive the sandbox that owned it.
	if _, ok := s.snapshot(id); !ok || ctx.Err() != nil {
		_ = s.p.Remove(context.WithoutCancel(ctx), id)
		return errGone
	}

	return nil
}

// placeExecd makes sure the execd volume for this architecture exists and runs in this image.
func (s *Server) placeExecd(ctx context.Context, inj provider.Injector, arch, image string) (string, error) {
	src, err := s.execd(ctx, arch)
	if err != nil {
		return "", err
	}

	// One at a time: two creates seeding the same volume would each copy into it, and the
	// second copy can land while the first sandbox is exec'ing the file.
	s.seedMu.Lock()
	defer s.seedMu.Unlock()

	if s.seeded[src.Volume] {
		return src.Volume, nil
	}

	if !inj.VolumeRuns(ctx, src.Volume, execdBinary, image) {
		var err error

		if src.File != "" {
			err = inj.SeedFile(ctx, src.Volume, execdBinary, src.File, image)
		} else {
			err = inj.SeedFromImage(ctx, src.Volume, src.Image, "/usr/local/bin")
		}

		if err != nil {
			return "", fmt.Errorf("placing the sandbox agent in volume %s: %w", src.Volume, err)
		}

		if !inj.VolumeRuns(ctx, src.Volume, execdBinary, image) {
			return "", fmt.Errorf("the sandbox agent in volume %s does not run inside %s - it may "+
				"be built for another architecture; `docker volume rm %s` and set "+
				"SBX_EXECD_BINARY to a linux/%s build", src.Volume, image, src.Volume, arch)
		}
	}

	s.seeded[src.Volume] = true

	return src.Volume, nil
}

// waitReady moves a sandbox from Pending to Running once execd answers through the daemon's
// port - the address the caller will be handed - or to Failed, with the reason and the last
// lines the container printed.
func (s *Server) waitReady(ctx context.Context, id string) {
	deadline := s.now().Add(s.readyTimeout)

	var lastErr error

	// Backing off from 5 ms rather than a flat 250: execd is usually listening within a few ms
	// of `docker run` returning, and a flat interval rounded every create up to it when the
	// first ping lost that race. Capped, because each round is a container list and a ping.
	wait := 5 * time.Millisecond

	for {
		if ctx.Err() != nil {
			return
		}

		units, err := s.unitsOf(ctx, id)
		if err != nil {
			lastErr = err
		} else if len(units) == 0 {
			if _, ok := s.snapshot(id); ok {
				s.update(id, func(r *record) {
					r.transition(stateFailed, "container_missing", "the container disappeared "+
						"before it became ready", s.now())
				})
			}

			return
		} else {
			u := units[0]

			// Checked before pinging, and the ping only happens while it is running: a ping
			// through the wake port would START an exited container, and the crash would look
			// like a slow boot until the deadline.
			if !u.Running && !u.Paused {
				s.update(id, func(r *record) {
					r.transition(stateFailed, "runtime_error", "the container exited before execd "+
						"answered. Its last output:\n"+s.tail(ctx, u.Ref), s.now())
				})

				return
			}

			if len(u.Client) > 0 {
				if lastErr = s.ping(ctx, u.Client[0].String()); lastErr == nil {
					s.trace.mark(id, "execd answered: Running")

					eps := make([]string, 0, len(u.Client))
					for _, c := range u.Client {
						eps = append(eps, c.String())
					}

					s.update(id, func(r *record) {
						if r.State == statePending {
							r.transition(stateRunning, "", "", s.now())
						}

						r.Endpoints = eps
					})

					s.mu.Lock()
					delete(s.provisioning, id)
					s.mu.Unlock()

					return
				}

				s.trace.mark(id, "execd ping failed: "+lastErr.Error())
			}
		}

		if s.now().After(deadline) {
			ref := ""
			if len(units) > 0 {
				ref = units[0].Ref
			}

			s.update(id, func(r *record) {
				r.transition(stateFailed, "provision_timeout", fmt.Sprintf("execd did not answer "+
					"/ping within %s (last error: %v). Its last output:\n%s",
					s.readyTimeout, lastErr, s.tail(ctx, ref)), s.now())
			})

			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		wait = min(2*wait, 250*time.Millisecond)
	}
}

func (s *Server) tail(ctx context.Context, ref string) string {
	if ref == "" {
		return "(no container)"
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	var b bytes.Buffer
	if err := s.p.Logs(ctx, ref, 20, false, &b); err != nil {
		return "(logs unavailable: " + err.Error() + ")"
	}

	return strings.TrimSpace(b.String())
}

// normalizeArch maps what an image reports to GOARCH spelling.
func normalizeArch(a string) string {
	switch a {
	case "aarch64", "arm64/v8":
		return "arm64"
	case "x86_64", "x86-64":
		return "amd64"
	}

	return a
}

// reap removes every sandbox whose expiry has passed.
func (s *Server) reap(ctx context.Context) {
	now := s.now()

	s.mu.Lock()

	var due []string

	for id, r := range s.recs {
		if r.ExpiresAt != nil && !now.Before(*r.ExpiresAt) && r.State != stateStopping {
			due = append(due, id)
		}
	}
	s.mu.Unlock()

	for _, id := range due {
		if err := s.remove(ctx, id, "expiry"); err != nil {
			logs.Default.Error(id, service, "osb: expired, but removing it failed: %v", err)
			continue
		}

		logs.Default.Info(id, service, "osb: expired and removed")
	}
}
