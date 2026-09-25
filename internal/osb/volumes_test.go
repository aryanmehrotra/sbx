package osb

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

func withVolumes(vols ...map[string]any) map[string]any {
	b := minimalCreate()

	list := make([]any, len(vols))
	for i, v := range vols {
		list[i] = v
	}

	b["volumes"] = list

	return b
}

// hostRoot gives a harness one allowed root, canonical (a Mac's TempDir is under a symlink).
func hostRoot(t *testing.T) (string, option) {
	t.Helper()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return root, func(_ *harness, o *Options) { o.HostPaths = []string{root} }
}

func TestHostVolumesBindOnlyUnderAllowedRoots(t *testing.T) {
	root, allow := hostRoot(t)
	h := newHarness(t, allow)

	sb := h.create(withVolumes(
		map[string]any{"name": "work", "host": map[string]any{"path": root + "/work"}, "mountPath": "/mnt/work"},
		map[string]any{"name": "ro", "host": map[string]any{"path": root}, "mountPath": "/mnt/ro",
			"readOnly": true, "subPath": "shared/data"},
	))

	got := h.p.service(sb.ID).VolumeMounts
	want := []spec.VolumeMount{
		{Host: root + "/work", Target: "/mnt/work"},
		{Host: root + "/shared/data", Target: "/mnt/ro", ReadOnly: true},
	}

	if !slices.Equal(got, want) {
		t.Fatalf("mounts = %+v, want %+v", got, want)
	}

	// Created on this machine as the user running sbx, rather than by docker as root.
	for _, d := range []string{root + "/work", root + "/shared/data"} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s was not created: %v", d, err)
		}
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	sibling := root + "2"

	refusals := []struct {
		name string
		vol  map[string]any
		code string
	}{
		{"outside", map[string]any{"name": "x", "host": map[string]any{"path": outside}, "mountPath": "/x"}, "VOLUME::HOST_PATH_NOT_ALLOWED"},
		{"prefix is not a parent", map[string]any{"name": "x", "host": map[string]any{"path": sibling}, "mountPath": "/x"}, "VOLUME::HOST_PATH_NOT_ALLOWED"},
		{"symlink out", map[string]any{"name": "x", "host": map[string]any{"path": root + "/escape"}, "mountPath": "/x"}, "VOLUME::HOST_PATH_NOT_ALLOWED"},
		{"missing, under a symlink out", map[string]any{"name": "x", "host": map[string]any{"path": root + "/escape/new/dir"}, "mountPath": "/x"}, "VOLUME::HOST_PATH_NOT_ALLOWED"},
		{"dotdot subPath", map[string]any{"name": "x", "host": map[string]any{"path": root}, "subPath": "../..", "mountPath": "/x"}, "VOLUME::INVALID_SUB_PATH"},
		{"relative path", map[string]any{"name": "x", "host": map[string]any{"path": "work"}, "mountPath": "/x"}, "VOLUME::INVALID_HOST_PATH"},
	}

	for _, r := range refusals {
		resp := h.do("POST", "/v1/sandboxes", withVolumes(r.vol), nil)
		if e := h.errOf(resp); resp.StatusCode != http.StatusBadRequest || e.Code != r.code {
			t.Errorf("%s: %d %+v, want 400 %s", r.name, resp.StatusCode, e, r.code)
		}
	}

	if _, err := os.Stat(sibling); err == nil {
		t.Errorf("%s was created although it is outside the allowed root", sibling)
	}

	// Checking a path that does not exist yet must not create it first: through the symlink, that
	// would make a directory outside the root before finding out it is outside the root.
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Errorf("%s/new was created through a symlink out of the allowed root", outside)
	}
}

func TestVolumeShapeRefusals(t *testing.T) {
	root, allow := hostRoot(t)
	h := newHarness(t, allow)

	host := map[string]any{"path": root}
	cases := []struct {
		name string
		vols []map[string]any
		code string
	}{
		{"ossfs", []map[string]any{{"name": "o", "ossfs": map[string]any{"bucket": "b"}, "mountPath": "/o"}}, "VOLUME::INVALID_BACKEND"},
		{"two backends", []map[string]any{{"name": "o", "host": host, "pvc": map[string]any{"claimName": "c"}, "mountPath": "/o"}}, "VOLUME::INVALID_BACKEND"},
		{"no backend", []map[string]any{{"name": "o", "mountPath": "/o"}}, "VOLUME::INVALID_BACKEND"},
		{"bad name", []map[string]any{{"name": "Data_1", "host": host, "mountPath": "/o"}}, "VOLUME::INVALID_NAME"},
		{"dup name", []map[string]any{{"name": "a", "host": host, "mountPath": "/o"}, {"name": "a", "host": host, "mountPath": "/p"}}, "VOLUME::DUPLICATE_NAME"},
		{"relative mount", []map[string]any{{"name": "a", "host": host, "mountPath": "o"}}, "VOLUME::INVALID_MOUNT_PATH"},
		{"over execd", []map[string]any{{"name": "a", "host": host, "mountPath": "/opt/sbx"}}, "VOLUME::INVALID_MOUNT_PATH"},
		{"same place", []map[string]any{{"name": "a", "host": host, "mountPath": "/o"}, {"name": "b", "host": host, "mountPath": "/o/"}}, "VOLUME::INVALID_MOUNT_PATH"},
		{"bad claim", []map[string]any{{"name": "a", "pvc": map[string]any{"claimName": "Bad/Name"}, "mountPath": "/o"}}, "VOLUME::INVALID_PVC_NAME"},
		{"unknown field", []map[string]any{{"name": "a", "host": host, "mountPath": "/o", "size": 1}}, "SANDBOX::INVALID_PARAMETER"},
		{"comma in path", []map[string]any{{"name": "a", "host": host, "mountPath": "/o,readonly=false"}}, "VOLUME::INVALID_MOUNT_PATH"},
	}

	for _, c := range cases {
		resp := h.do("POST", "/v1/sandboxes", withVolumes(c.vols...), nil)
		if e := h.errOf(resp); resp.StatusCode != http.StatusBadRequest || e.Code != c.code {
			t.Errorf("%s: %d %+v, want 400 %s", c.name, resp.StatusCode, e, c.code)
		}
	}
}

func TestPVCIsANamespacedDockerVolume(t *testing.T) {
	h := newHarness(t)

	sb := h.create(withVolumes(
		map[string]any{"name": "data", "pvc": map[string]any{"claimName": "datasets"}, "mountPath": "/data",
			"subPath": "train", "readOnly": true},
		map[string]any{"name": "scratch", "pvc": map[string]any{"claimName": "scratch", "deleteOnSandboxTermination": true},
			"mountPath": "/scratch"},
	))

	got := h.p.service(sb.ID).VolumeMounts
	want := []spec.VolumeMount{
		{Volume: "sbx-osb-pvc-datasets", Target: "/data", SubPath: "train", ReadOnly: true},
		{Volume: "sbx-osb-pvc-scratch", Target: "/scratch"},
	}

	if !slices.Equal(got, want) {
		t.Fatalf("mounts = %+v, want %+v", got, want)
	}

	if c := h.p.lists(&h.p.volCreated); !slices.Equal(c, []string{"sbx-osb-pvc-datasets", "sbx-osb-pvc-scratch"}) {
		t.Fatalf("created = %v", c)
	}

	// deleteOnSandboxTermination removes the one it applies to, and only that one.
	h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, nil)

	if rm := h.p.lists(&h.p.volRemoved); !slices.Equal(rm, []string{"sbx-osb-pvc-scratch"}) {
		t.Fatalf("removed = %v, want only the deleteOnSandboxTermination volume", rm)
	}
}

// A volume that existed before the create is somebody's data: never created, never deleted,
// whatever deleteOnSandboxTermination says.
func TestPreexistingPVCIsNeverDeleted(t *testing.T) {
	h := newHarness(t)

	h.p.mu.Lock()
	h.p.volumes = map[string]bool{"sbx-osb-pvc-shared": true}
	h.p.mu.Unlock()

	sb := h.create(withVolumes(map[string]any{"name": "s", "mountPath": "/s",
		"pvc": map[string]any{"claimName": "shared", "createIfNotExists": false, "deleteOnSandboxTermination": true}}))

	h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, nil)

	if c, rm := h.p.lists(&h.p.volCreated), h.p.lists(&h.p.volRemoved); len(c)+len(rm) != 0 {
		t.Fatalf("created %v, removed %v - a pre-existing volume was touched", c, rm)
	}
}

func TestMissingPVCWithoutCreateIsRefusedAndUndone(t *testing.T) {
	h := newHarness(t)

	resp := h.do("POST", "/v1/sandboxes", withVolumes(
		map[string]any{"name": "a", "pvc": map[string]any{"claimName": "fresh"}, "mountPath": "/a"},
		map[string]any{"name": "b", "pvc": map[string]any{"claimName": "absent", "createIfNotExists": false}, "mountPath": "/b"},
	), nil)

	e := h.errOf(resp)
	if resp.StatusCode != http.StatusBadRequest || e.Code != "VOLUME::PVC_NOT_FOUND" ||
		!strings.Contains(e.Message, "docker volume create sbx-osb-pvc-absent") {
		t.Fatalf("= %d %+v", resp.StatusCode, e)
	}

	// The first volume was created by this request and no sandbox will ever own it.
	if rm := h.p.lists(&h.p.volRemoved); !slices.Equal(rm, []string{"sbx-osb-pvc-fresh"}) {
		t.Fatalf("removed = %v, want the volume this refused request created", rm)
	}

	var l listJSON
	if h.do("GET", "/v1/sandboxes", nil, &l); len(l.Items) != 0 {
		t.Errorf("a refused create left a sandbox: %+v", l.Items)
	}
}

func TestCheckHostPaths(t *testing.T) {
	for _, bad := range [][]string{{"/"}, {"relative"}, {"/ok", "/"}} {
		if CheckHostPaths(bad) == nil {
			t.Errorf("%v accepted", bad)
		}
	}

	if err := CheckHostPaths([]string{"/Users/me/sandboxes", "/srv/data"}); err != nil {
		t.Error(err)
	}
}

// The mounts are never written anywhere sbx reads back: they are in the container docker
// already has, and in the record only as the pvc volumes this sandbox owns. So a restarted
// server must drive a sandbox with volumes - pause, resume, delete and its owned-volume
// cleanup - without re-deriving a spec, and without recreating the container.
func TestVolumesSurviveARestartWithoutASpec(t *testing.T) {
	root, allow := hostRoot(t)
	h := newHarness(t, allow)

	sb := h.create(withVolumes(
		map[string]any{"name": "scratch", "pvc": map[string]any{"claimName": "scratch", "deleteOnSandboxTermination": true},
			"mountPath": "/scratch"},
		map[string]any{"name": "work", "host": map[string]any{"path": root + "/work"}, "mountPath": "/work"},
	))

	before := h.p.service(sb.ID)

	h.http.Close()
	h.srv.Close()
	h.rt = &fakeRuntime{}
	h.start(allow)

	ctx, cancel := context.WithCancel(t.Context())
	go h.srv.Run(ctx)

	defer cancel()

	if resp := h.do("POST", "/v1/sandboxes/"+sb.ID+"/pause", nil, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pause after restart: %d", resp.StatusCode)
	}

	h.waitState(sb.ID, statePaused)

	if resp := h.do("POST", "/v1/sandboxes/"+sb.ID+"/resume", nil, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("resume after restart: %d", resp.StatusCode)
	}

	h.waitState(sb.ID, stateRunning)

	after := h.p.service(sb.ID)
	if !slices.Equal(after.VolumeMounts, before.VolumeMounts) || len(after.ReadOnlyVolumes) != 1 {
		t.Fatalf("the container was re-specified across a restart: %+v -> %+v", before, after)
	}

	h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, nil)

	if rm := h.p.lists(&h.p.volRemoved); !slices.Equal(rm, []string{"sbx-osb-pvc-scratch"}) {
		t.Fatalf("owned volume after a restart: removed = %v", rm)
	}
}

// The host path is checked when the request arrives and mounted when docker runs, seconds
// later. A directory swapped for a symlink out of the allowed root in between must be refused
// at create, not followed by docker.
func TestHostPathSwappedForASymlinkBeforeCreateIsRefused(t *testing.T) {
	root, allow := hostRoot(t)
	h := newHarness(t, allow)

	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()

	h.p.mu.Lock()
	h.p.onInspect = func() {
		if err := os.Rename(work, work+".old"); err != nil {
			t.Error(err)
		}

		if err := os.Symlink(outside, work); err != nil {
			t.Error(err)
		}
	}
	h.p.mu.Unlock()

	sb := h.create(withVolumes(map[string]any{"name": "work", "host": map[string]any{"path": work}, "mountPath": "/work"}))

	if sb.Status.State != stateFailed || sb.Status.Reason != "host_path_changed" {
		t.Fatalf("state = %s (%s: %s), want Failed host_path_changed", sb.Status.State, sb.Status.Reason, sb.Status.Message)
	}

	if svc := h.p.service(sb.ID); len(svc.VolumeMounts) != 0 {
		t.Fatalf("a container was created with %+v", svc.VolumeMounts)
	}
}

// A refused create leaves nothing behind: not a host directory made while validating a request
// that a later check refused.
func TestRefusedCreateMakesNoHostDirectory(t *testing.T) {
	root, allow := hostRoot(t)
	h := newHarness(t, allow)

	b := withVolumes(map[string]any{"name": "work", "host": map[string]any{"path": root + "/fresh/work"}, "mountPath": "/work"})
	b["timeout"] = 30 // refused after volumes are parsed

	if resp := h.do("POST", "/v1/sandboxes", b, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create = %d, want 400", resp.StatusCode)
	}

	if _, err := os.Stat(root + "/fresh"); !os.IsNotExist(err) {
		t.Fatalf("%s/fresh exists after a refused create: %v", root, err)
	}
}

// Nor pvc volumes made for a create whose record could not be saved: no sandbox will ever own
// them, so nothing would ever remove them.
func TestUnsavedCreateRemovesTheVolumesItMade(t *testing.T) {
	h := newHarness(t)

	h.p.mu.Lock()
	h.p.volumes = map[string]bool{"sbx-osb-pvc-kept": true}
	h.p.mu.Unlock()

	if err := os.Chmod(h.dir, 0o500); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(h.dir, 0o700) })

	resp := h.do("POST", "/v1/sandboxes", withVolumes(
		map[string]any{"name": "a", "pvc": map[string]any{"claimName": "fresh"}, "mountPath": "/a"},
		map[string]any{"name": "b", "pvc": map[string]any{"claimName": "kept"}, "mountPath": "/b"},
	), nil)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("create = %d, want 500 (the record could not be saved)", resp.StatusCode)
	}

	if rm := h.p.lists(&h.p.volRemoved); !slices.Equal(rm, []string{"sbx-osb-pvc-fresh"}) {
		t.Fatalf("removed = %v, want only the volume this create made", rm)
	}
}
