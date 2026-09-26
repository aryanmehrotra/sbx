package osb

// Volumes on create: OpenSandbox's `volumes`, mapped onto what docker has.
//
//   host  -> a bind mount, only under a root the operator listed with --osb-host-paths.
//   pvc   -> a docker named volume, sbx-osb-pvc-<claimName>.
//   ossfs -> refused: Alibaba OSS, out of scope.
//
// Everything here runs synchronously in the create call, before the 202: a path outside the
// allow-list, a claim that must exist and does not, a subPath with '..' - each is a 400 now
// rather than a Failed sandbox a poll later. Upstream's docker runtime makes the same checks in
// the same place (services/docker/volumes.py at release-1.1.0).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

type volumeJSON struct {
	Name      string          `json:"name"`
	Host      *hostJSON       `json:"host"`
	PVC       *pvcJSON        `json:"pvc"`
	OSSFS     json.RawMessage `json:"ossfs"`
	MountPath string          `json:"mountPath"`
	ReadOnly  bool            `json:"readOnly"`
	SubPath   string          `json:"subPath"`
}

type hostJSON struct {
	Path string `json:"path"`
}

type pvcJSON struct {
	ClaimName                  string `json:"claimName"`
	CreateIfNotExists          *bool  `json:"createIfNotExists"`
	DeleteOnSandboxTermination *bool  `json:"deleteOnSandboxTermination"`

	// Kubernetes provisioning hints. The spec says docker ignores them, and so does sbx - they
	// are decoded only so that sending them is not an unknown-field error.
	StorageClass *string  `json:"storageClass"`
	Storage      *string  `json:"storage"`
	AccessModes  []string `json:"accessModes"`
}

// dnsLabel is the spec's pattern for a volume name and a claim name.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// pvcVolume is the docker volume a claim maps to. Namespaced, deliberately unlike upstream,
// which uses claimName as the docker volume name verbatim: that would let any API caller mount
// ANY volume on the engine - another sandbox's database, the execd volume, something that has
// nothing to do with sbx - by naming it. With the prefix, the API reaches only volumes it made
// or that somebody deliberately made in its namespace (`docker volume create sbx-osb-pvc-x`).
func pvcVolume(claim string) string { return provider.PVCVolumePrefix + claim }

// pvcLabel marks a volume the API created, so a person reading `docker volume ls` can tell.
const pvcLabel = "sbx.osb.pvc"

// volumeErr is a refusal with the spec's status and code.
type volumeErr struct {
	status int
	code   string
	msg    string
}

func (e *volumeErr) Error() string { return e.msg }

func volBad(code, msg string, args ...any) *volumeErr {
	return &volumeErr{status: http.StatusBadRequest, code: code, msg: fmt.Sprintf(msg, args...)}
}

// parseVolumes checks everything that needs no docker: shapes, names, paths, the allow-list.
// It returns the mounts to make, with pvc volumes not yet known to exist.
func (s *Server) parseVolumes(raw []json.RawMessage) ([]spec.VolumeMount, []pvcJSON, *volumeErr) {
	var (
		mounts []spec.VolumeMount
		claims []pvcJSON
		names  = map[string]bool{}
		paths  = map[string]bool{}
	)

	for i, r := range raw {
		var v volumeJSON

		dec := json.NewDecoder(bytes.NewReader(r))
		dec.DisallowUnknownFields()

		if err := dec.Decode(&v); err != nil {
			// ossfs first: a well-formed ossfs volume with a field sbx does not model should
			// still be told the backend is the problem, not the field.
			var probe map[string]json.RawMessage
			if json.Unmarshal(r, &probe) == nil && probe["ossfs"] != nil {
				return nil, nil, ossfsRefusal()
			}

			return nil, nil, volBad("SANDBOX::INVALID_PARAMETER", "volumes[%d] is not a Volume: %v", i, err)
		}

		backends := 0
		for _, set := range []bool{v.Host != nil, v.PVC != nil, len(v.OSSFS) > 0 && string(v.OSSFS) != "null"} {
			if set {
				backends++
			}
		}

		switch {
		case len(v.OSSFS) > 0 && string(v.OSSFS) != "null":
			return nil, nil, ossfsRefusal()
		case !dnsLabel.MatchString(v.Name) || len(v.Name) > 63:
			return nil, nil, volBad("VOLUME::INVALID_NAME", "volumes[%d].name %q must be a DNS label: "+
				"lowercase letters, digits and '-', at most 63 characters", i, v.Name)
		case names[v.Name]:
			return nil, nil, volBad("VOLUME::DUPLICATE_NAME", "volume name %q is used twice", v.Name)
		case backends != 1:
			return nil, nil, volBad("VOLUME::INVALID_BACKEND", "volume %q needs exactly one backend: "+
				"host or pvc", v.Name)
		case !strings.HasPrefix(v.MountPath, "/"):
			return nil, nil, volBad("VOLUME::INVALID_MOUNT_PATH", "volume %q: mountPath %q must be "+
				"an absolute path in the container", v.Name, v.MountPath)
		}

		target := path.Clean(v.MountPath)

		// /opt/sbx is where execd lives. A volume there would hide the agent the sandbox runs
		// under, and the sandbox would never become ready - with no hint why.
		if target == "/" || target == execdMount || strings.HasPrefix(target, execdMount+"/") {
			return nil, nil, volBad("VOLUME::INVALID_MOUNT_PATH", "volume %q: mountPath %s is "+
				"reserved - / would replace the whole filesystem and %s holds the sandbox agent",
				v.Name, target, execdMount)
		}

		if paths[target] {
			return nil, nil, volBad("VOLUME::INVALID_MOUNT_PATH", "two volumes mount at %s", target)
		}

		if err := checkSubPath(v.SubPath); err != nil {
			return nil, nil, volBad("VOLUME::INVALID_SUB_PATH", "volume %q: %v", v.Name, err)
		}

		names[v.Name], paths[target] = true, true

		if v.Host != nil {
			src, verr := s.hostSource(v)
			if verr != nil {
				return nil, nil, verr
			}

			mounts = append(mounts, spec.VolumeMount{Host: src, Target: target, ReadOnly: v.ReadOnly})

			continue
		}

		if !dnsLabel.MatchString(v.PVC.ClaimName) || len(v.PVC.ClaimName) > 253 {
			return nil, nil, volBad("VOLUME::INVALID_PVC_NAME", "volume %q: claimName %q must be a "+
				"DNS label", v.Name, v.PVC.ClaimName)
		}

		mounts = append(mounts, spec.VolumeMount{Volume: pvcVolume(v.PVC.ClaimName), Target: target,
			SubPath: path.Clean(v.SubPath), ReadOnly: v.ReadOnly})

		if v.SubPath == "" {
			mounts[len(mounts)-1].SubPath = ""
		}

		claims = append(claims, *v.PVC)
	}

	// A comma or a quote cannot be carried by docker's --mount; say so as a 400 here rather
	// than failing the sandbox later.
	for _, m := range mounts {
		probe := spec.Service{Image: "x", Ports: []int{1}, VolumeMounts: []spec.VolumeMount{m}}
		if err := probe.Validate("sandbox"); err != nil {
			return nil, nil, volBad("VOLUME::INVALID_MOUNT_PATH", "%v", err)
		}
	}

	return mounts, claims, nil
}

func ossfsRefusal() *volumeErr {
	return volBad("VOLUME::INVALID_BACKEND", "ossfs volumes mount Alibaba OSS buckets, which sbx "+
		"does not support and does not plan to - use a host or pvc volume")
}

func checkSubPath(sub string) error {
	if sub == "" {
		return nil
	}

	if strings.HasPrefix(sub, "/") || filepath.IsAbs(sub) {
		return fmt.Errorf("subPath %q must be relative", sub)
	}

	if slices.Contains(strings.Split(filepath.ToSlash(sub), "/"), "..") {
		return fmt.Errorf("subPath %q must not contain '..'", sub)
	}

	return nil
}

// hostSource resolves a host volume to the directory to bind, or refuses it. It changes
// nothing on disk: it runs while the request is still being validated, and a request refused by
// a later check must leave no directory behind. makeHostDirs creates what is missing once the
// whole request has passed.
//
// Lexically first, then again after following symlinks: a link inside an allowed root that
// points at / passes a string check and escapes at mount time, because docker follows it. A
// path that does not exist yet is resolved through its nearest existing ancestor - the part a
// symlink could be in - and the canonical path is what gets mounted, so the check and the mount
// are about the same directory.
func (s *Server) hostSource(v volumeJSON) (string, *volumeErr) {
	// First: no allow-list can make a mount the provider has no way to do.
	if err := provider.HostVolumesFor(s.p); err != nil {
		return "", &volumeErr{status: http.StatusNotImplemented, code: "SANDBOX::API_NOT_SUPPORTED",
			msg: fmt.Sprintf("volume %q (host.path): %v", v.Name, err)}
	}

	if len(s.hostPaths) == 0 {
		return "", &volumeErr{status: http.StatusBadRequest, code: "VOLUME::HOST_PATH_NOT_ALLOWED",
			msg: fmt.Sprintf("volume %q: host volumes are off on this server. Start it with "+
				"`sbx serve --osb-host-paths /some/root` (comma-separated for several) to allow "+
				"bind mounts of directories under those roots", v.Name)}
	}

	p := v.Host.Path
	if !filepath.IsAbs(p) {
		return "", volBad("VOLUME::INVALID_HOST_PATH", "volume %q: host.path %q must be absolute", v.Name, p)
	}

	p = filepath.Clean(filepath.Join(p, v.SubPath))

	if !underAny(p, s.hostPaths) {
		return "", notAllowed(v.Name, p, s.hostPaths)
	}

	canonical, err := resolveExisting(p)
	if err != nil {
		return "", &volumeErr{status: http.StatusInternalServerError, code: "VOLUME::HOST_PATH_CREATE_FAILED",
			msg: fmt.Sprintf("volume %q: resolving %s: %v", v.Name, p, err)}
	}

	if !underAny(canonical, s.hostRoots()) {
		return "", notAllowed(v.Name, canonical+" (where "+p+" leads)", s.hostPaths)
	}

	return canonical, nil
}

// resolveExisting is filepath.EvalSymlinks for a path that may not exist yet: the nearest
// ancestor that does exist is resolved, and the missing components - which cannot be symlinks,
// since they are not anything - are appended to it.
func resolveExisting(p string) (string, error) {
	var missing []string

	for cur := p; ; cur = filepath.Dir(cur) {
		if _, err := os.Lstat(cur); err == nil {
			base, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}

			slices.Reverse(missing)

			return filepath.Join(append([]string{base}, missing...)...), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}

		if cur == filepath.Dir(cur) {
			return "", fmt.Errorf("no part of %s exists", p)
		}

		missing = append(missing, filepath.Base(cur))
	}
}

// makeHostDirs creates the host directories a validated request mounts - as the user running
// sbx, not as root, which is what docker would do - and checks each still resolves to itself
// under an allowed root. The returned undo removes the directories this call created, deepest
// first and only while empty, for a create that fails after it.
func (s *Server) makeHostDirs(mounts []spec.VolumeMount) (func(), *volumeErr) {
	var made []string

	undo := func() {
		for _, d := range slices.Backward(made) {
			_ = os.Remove(d)
		}
	}

	for _, m := range mounts {
		if m.Host == "" {
			continue
		}

		var fresh []string

		for cur := m.Host; ; cur = filepath.Dir(cur) {
			if _, err := os.Lstat(cur); err == nil || cur == filepath.Dir(cur) {
				break
			}

			fresh = append(fresh, cur)
		}

		if err := os.MkdirAll(m.Host, 0o755); err != nil {
			undo()

			return nil, &volumeErr{status: http.StatusInternalServerError, code: "VOLUME::HOST_PATH_CREATE_FAILED",
				msg: fmt.Sprintf("could not create %s on the machine running sbx serve: %v", m.Host, err)}
		}

		slices.Reverse(fresh) // parents first, so undo removes children first
		made = append(made, fresh...)

		if err := s.checkHostMount(m.Host); err != nil {
			undo()
			return nil, volBad("VOLUME::HOST_PATH_NOT_ALLOWED", "%v", err)
		}
	}

	return undo, nil
}

// checkHostMount is the allow-list check again, for a path that was canonical when the request
// was validated: it must still be a directory, still resolve to exactly itself, and still be
// under an allowed root. A component swapped for a symlink since then changes what it resolves
// to, and docker would follow it.
func (s *Server) checkHostMount(p string) error {
	now, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("host path %s changed since the request was checked: %w", p, err)
	}

	if now != p {
		return fmt.Errorf("host path %s changed since the request was checked: it now leads to %s, "+
			"and sbx mounts only the directory it checked", p, now)
	}

	if !underAny(now, s.hostRoots()) {
		return fmt.Errorf("host path %s is no longer under a root this server allows (%s)",
			p, strings.Join(s.hostPaths, ", "))
	}

	if fi, err := os.Stat(now); err != nil || !fi.IsDir() {
		return fmt.Errorf("host path %s is no longer a directory", p)
	}

	return nil
}

func notAllowed(name, p string, roots []string) *volumeErr {
	return volBad("VOLUME::HOST_PATH_NOT_ALLOWED", "volume %q: %s is not under a root this "+
		"server allows (%s) - pick a directory under one of them, or restart sbx serve with "+
		"--osb-host-paths including it", name, p, strings.Join(roots, ", "))
}

// hostRoots is the allow-list with symlinks resolved, so /tmp and /private/tmp on a Mac are
// the same root. A root that cannot be resolved stays as written.
func (s *Server) hostRoots() []string {
	out := make([]string, 0, len(s.hostPaths))

	for _, r := range s.hostPaths {
		if c, err := filepath.EvalSymlinks(r); err == nil {
			r = c
		}

		out = append(out, r)
	}

	return out
}

// underAny reports whether p is one of roots or inside one - on a path boundary, so /data2 is
// not inside /data.
func underAny(p string, roots []string) bool {
	for _, r := range roots {
		r = filepath.Clean(r)
		if p == r || strings.HasPrefix(p, strings.TrimSuffix(r, string(filepath.Separator))+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// CheckHostPaths validates --osb-host-paths at startup: absolute, and not the filesystem root,
// which would make the allow-list a formality.
func CheckHostPaths(roots []string) error {
	for _, r := range roots {
		if !filepath.IsAbs(r) {
			return fmt.Errorf("--osb-host-paths %q must be an absolute directory", r)
		}

		if filepath.Clean(r) == string(filepath.Separator) {
			return errors.New("--osb-host-paths / would allow every directory on this machine; " +
				"list the roots sandboxes may mount instead")
		}
	}

	return nil
}

// ensureClaims makes sure every pvc volume exists, creating those allowed to be created. It
// returns every volume it created, and the subset the caller asked to have removed with the
// sandbox. If it fails part way it removes what it created itself; if the create fails after it
// returns, the caller removes `created` (removeVolumes), since no sandbox will ever own them.
func (s *Server) ensureClaims(ctx context.Context, claims []pvcJSON) (created, owned []string, _ *volumeErr) {
	if len(claims) == 0 {
		return nil, nil, nil
	}

	nv, err := provider.NamedVolumesFor(s.p)
	if err != nil {
		return nil, nil, &volumeErr{status: http.StatusNotImplemented, code: "SANDBOX::API_NOT_SUPPORTED", msg: err.Error()}
	}

	undo := func() { s.removeVolumes(ctx, created) }

	seen := map[string]bool{}

	for _, c := range claims {
		name := pvcVolume(c.ClaimName)
		if seen[name] {
			continue
		}

		seen[name] = true

		exists, err := nv.VolumeExists(ctx, name)
		if err != nil {
			undo()
			return nil, nil, &volumeErr{status: http.StatusInternalServerError, code: "VOLUME::PVC_INSPECT_FAILED",
				msg: fmt.Sprintf("claim %q: inspecting docker volume %s: %v", c.ClaimName, name, err)}
		}

		if exists {
			continue
		}

		if c.CreateIfNotExists != nil && !*c.CreateIfNotExists {
			undo()
			return nil, nil, volBad("VOLUME::PVC_NOT_FOUND", "claim %q: docker volume %s does not exist "+
				"and createIfNotExists is false - create it with `docker volume create %s`, or "+
				"let sbx create it", c.ClaimName, name, name)
		}

		if err := nv.CreateVolume(ctx, name, map[string]string{pvcLabel: c.ClaimName}); err != nil {
			undo()
			return nil, nil, &volumeErr{status: http.StatusInternalServerError, code: "VOLUME::PVC_INSPECT_FAILED",
				msg: fmt.Sprintf("claim %q: creating docker volume %s: %v", c.ClaimName, name, err)}
		}

		created = append(created, name)

		// Only a volume made by THIS request is ever deleted with the sandbox; a pre-existing
		// one is somebody's data, whatever the flag says. That is the spec's rule too.
		if c.DeleteOnSandboxTermination != nil && *c.DeleteOnSandboxTermination {
			owned = append(owned, name)
		}
	}

	return created, owned, nil
}

// removeVolumes removes pvc volumes made for a create that then failed. Best effort, and
// logged: the create is already being refused, and a volume left behind is named in the log.
func (s *Server) removeVolumes(ctx context.Context, names []string) {
	nv, err := provider.NamedVolumesFor(s.p)
	if err != nil {
		return
	}

	for _, name := range names {
		if err := nv.RemoveVolume(context.WithoutCancel(ctx), name); err != nil {
			logs.Default.Warn("", "", "osb: could not remove volume %s after a refused create: %v - "+
				"`docker volume rm %s`", name, err, name)
		}
	}
}

// releaseClaims removes the volumes a sandbox owned. Best effort: a volume another sandbox
// still mounts is refused by docker, and that refusal is right - it is logged, not forced.
func (s *Server) releaseClaims(ctx context.Context, id string, names []string) {
	if len(names) == 0 {
		return
	}

	nv, err := provider.NamedVolumesFor(s.p)
	if err != nil {
		return
	}

	for _, name := range names {
		if err := nv.RemoveVolume(ctx, name); err != nil {
			logs.Default.Warn(id, service, "osb: could not remove volume %s (deleteOnSandboxTermination): "+
				"%v - `docker volume rm %s` once nothing mounts it", name, err, name)
		}
	}
}
