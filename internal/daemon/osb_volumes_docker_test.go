package daemon

// Docker-backed: snapshots and volumes through the OpenSandbox API against a real engine.
// Fenced the same way as osb_docker_test.go - a daemon scoped to ids this test mints, and
// cleanup of only the containers, images and volumes it named.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/osb"
)

type osbTestAPI struct {
	t      *testing.T
	url    string
	prefix string
}

// startOSB builds the linux agent, starts a scoped daemon and an API over it.
func startOSB(t *testing.T, hostPaths []string) *osbTestAPI {
	t.Helper()

	p := dockerOrSkip(t)
	log.SetOutput(io.Discard)

	arch := strings.TrimSpace(dockerCLI(t, "info", "--format", "{{.Architecture}}"))
	switch arch {
	case "aarch64":
		arch = "arm64"
	case "x86_64":
		arch = "amd64"
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "sbx-linux")

	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the linux agent: %v: %s", err, out)
	}

	t.Setenv("SBX_EXECD_BINARY", bin)
	t.Setenv("SBX_HISTORY", filepath.Join(tmp, "history.jsonl"))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	prefix := fmt.Sprintf("osb-%06x", time.Now().UnixNano()&0xffffff)

	// A long idle window: these tests are about the filesystem, and a freeze in the middle of
	// a `docker exec` would only add a way to be slow.
	d := New(p, 5*time.Minute, 60*time.Second, time.Second)
	d.scope = Scope{prefix}

	go d.run(ctx)

	api, err := osb.New(osb.Options{Provider: p, Runtime: d, StateDir: filepath.Join(tmp, "osb"),
		Version: "dev-test", ReadyTimeout: 90 * time.Second, HostPaths: hostPaths,
		NewID: func() string { return fmt.Sprintf("%s%06x", prefix, time.Now().UnixNano()&0xffffff) }})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(api.Close)

	return &osbTestAPI{t: t, url: srv.URL, prefix: prefix}
}

func (a *osbTestAPI) call(method, path, body string, out any) int {
	a.t.Helper()

	req, _ := http.NewRequest(method, a.url+path, strings.NewReader(body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if out != nil {
		_ = json.Unmarshal(raw, out)
	}

	if resp.StatusCode >= 400 {
		a.t.Logf("%s %s = %d %s", method, path, resp.StatusCode, raw)
	}

	return resp.StatusCode
}

// create makes a sandbox, registers its container for cleanup and waits for Running.
func (a *osbTestAPI) create(body string) string {
	a.t.Helper()

	var created struct{ ID string }
	if s := a.call("POST", "/v1/sandboxes", body, &created); s != 202 {
		a.t.Fatalf("create = %d", s)
	}

	id := created.ID
	a.t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "sbx-"+id+"-sandbox").Run() })

	deadline := time.Now().Add(120 * time.Second)

	for {
		var sb struct {
			Status struct{ State, Reason, Message string }
		}
		a.call("GET", "/v1/sandboxes/"+id, "", &sb)

		if sb.Status.State == "Running" {
			return id
		}

		if sb.Status.State == "Failed" || time.Now().After(deadline) {
			a.t.Fatalf("%s is %s (%s: %s)", id, sb.Status.State, sb.Status.Reason, sb.Status.Message)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

func inSandbox(t *testing.T, id, script string) (string, error) {
	t.Helper()

	out, err := exec.Command("docker", "exec", "sbx-"+id+"-sandbox", "sh", "-c", script).CombinedOutput()

	return strings.TrimSpace(string(out)), err
}

// The point of a snapshot: a file written before it is in the sandbox restored from it, and
// the restored sandbox is a different sandbox with its own agent answering.
func TestDockerSnapshotForkSeesTheFileWrittenBeforeIt(t *testing.T) {
	a := startOSB(t, nil)

	src := a.create(`{"image":{"uri":"alpine:3.20"},"entrypoint":["sleep","3600"],"timeout":600,
		"resourceLimits":{"cpu":"500m","memory":"128Mi"}}`)

	marker := fmt.Sprintf("written-before-the-snapshot-%d", time.Now().UnixNano())
	if out, err := inSandbox(t, src, "mkdir -p /srv/state && echo "+marker+" > /srv/state/marker"); err != nil {
		t.Fatalf("writing the marker: %v: %s", err, out)
	}

	var snap struct {
		ID     string
		Status struct{ State, Reason, Message string }
	}

	if s := a.call("POST", "/v1/sandboxes/"+src+"/snapshots", `{"name":"with-marker"}`, &snap); s != 202 {
		t.Fatalf("snapshot = %d", s)
	}

	image := "sbx-osb-snap:" + snap.ID
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", image).Run() })

	deadline := time.Now().Add(120 * time.Second)
	for snap.Status.State != "Ready" {
		if snap.Status.State == "Failed" || time.Now().After(deadline) {
			t.Fatalf("snapshot %s is %s: %s", snap.ID, snap.Status.State, snap.Status.Message)
		}

		time.Sleep(200 * time.Millisecond)
		a.call("GET", "/v1/snapshots/"+snap.ID, "", &snap)
	}

	// Commit paused the source for the copy and docker resumed it.
	if st := containerState("sbx-" + src + "-sandbox"); st != "running" {
		t.Fatalf("after the snapshot the source is %s, want running", st)
	}

	// Written after the snapshot: must NOT be in the fork.
	_, _ = inSandbox(t, src, "echo later > /srv/state/after")

	fork := a.create(`{"snapshotId":"` + snap.ID + `","timeout":600,
		"resourceLimits":{"cpu":"500m","memory":"128Mi"}}`)

	if fork == src {
		t.Fatal("the fork has the source's id")
	}

	got, err := inSandbox(t, fork, "cat /srv/state/marker")
	if err != nil || got != marker {
		t.Fatalf("the fork's /srv/state/marker = %q (%v), want %q", got, err, marker)
	}

	if out, err := inSandbox(t, fork, "test ! -e /srv/state/after && echo absent"); err != nil || out != "absent" {
		t.Fatalf("a file written after the snapshot reached the fork: %q %v", out, err)
	}

	// The fork's own agent answers through its own endpoint, with its own token.
	var ep struct {
		Endpoint string
		Headers  map[string]string
	}

	if a.call("GET", "/v1/sandboxes/"+fork+"/endpoints/44772", "", &ep) != 200 {
		t.Fatal("no execd endpoint for the fork")
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get("http://" + ep.Endpoint + "/ping")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("the fork's execd: %v %v", resp, err)
	}

	_ = resp.Body.Close()

	// In use, the snapshot cannot be deleted; with its sandboxes gone, it can, and the image goes.
	if s := a.call("DELETE", "/v1/snapshots/"+snap.ID, "", nil); s != 409 {
		t.Fatalf("DELETE of a snapshot a sandbox runs on = %d, want 409", s)
	}

	for _, id := range []string{fork, src} {
		if s := a.call("DELETE", "/v1/sandboxes/"+id, "", nil); s != 204 {
			t.Fatalf("delete %s = %d", id, s)
		}
	}

	if s := a.call("DELETE", "/v1/snapshots/"+snap.ID, "", nil); s != 204 {
		t.Fatalf("DELETE snapshot = %d", s)
	}

	if err := exec.Command("docker", "image", "inspect", image).Run(); err == nil {
		t.Fatalf("%s still exists after the snapshot was deleted", image)
	}
}

// Host and pvc volumes against a real engine: read-write, read-only, a pvc subPath, and
// deleteOnSandboxTermination.
func TestDockerHostAndPVCVolumes(t *testing.T) {
	// Under $HOME rather than a temp dir: a VM-backed engine (colima, Docker Desktop) shares
	// the home directory, and a host path it cannot see is refused by --mount - correctly, and
	// not what this test is about.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	root, err := os.MkdirTemp(filepath.Join(home, ".cache"), "sbx-osb-hostvol-")
	if err != nil {
		t.Skipf("cannot make a directory under %s/.cache: %v", home, err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(root) })

	if err := os.MkdirAll(filepath.Join(root, "ro"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "ro", "seed"), []byte("from-the-host\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := startOSB(t, []string{root})

	suffix := fmt.Sprintf("%x", time.Now().UnixNano())[:10]
	fresh, seeded := "t-fresh-"+suffix, "t-seeded-"+suffix
	freshVol, seededVol := "sbx-osb-pvc-"+fresh, "sbx-osb-pvc-"+seeded

	t.Cleanup(func() {
		_ = exec.Command("docker", "volume", "rm", "-f", freshVol).Run()
		_ = exec.Command("docker", "volume", "rm", "-f", seededVol).Run()
	})

	// A volume that exists before the create, with a subdirectory to mount by subPath.
	dockerCLI(t, "volume", "create", seededVol)
	dockerCLI(t, "run", "--rm", "-v", seededVol+":/v", "alpine:3.20", "sh", "-c",
		"mkdir -p /v/train && echo in-the-subpath > /v/train/data && echo at-the-root > /v/top")

	id := a.create(`{"image":{"uri":"alpine:3.20"},"entrypoint":["sleep","3600"],"timeout":600,
		"resourceLimits":{"cpu":"500m","memory":"128Mi"},"volumes":[
		{"name":"work","host":{"path":"` + root + `"},"subPath":"work","mountPath":"/mnt/work"},
		{"name":"seed","host":{"path":"` + root + `/ro"},"mountPath":"/mnt/seed","readOnly":true},
		{"name":"scratch","pvc":{"claimName":"` + fresh + `","deleteOnSandboxTermination":true},"mountPath":"/mnt/scratch"},
		{"name":"train","pvc":{"claimName":"` + seeded + `","createIfNotExists":false,"deleteOnSandboxTermination":true},
		 "mountPath":"/mnt/train","subPath":"train","readOnly":true}]}`)

	// host, read-write: a file written inside is on this machine's disk.
	if out, err := inSandbox(t, id, "echo from-the-sandbox > /mnt/work/out"); err != nil {
		t.Fatalf("writing to the host volume: %v %s", err, out)
	}

	if b, err := os.ReadFile(filepath.Join(root, "work", "out")); err != nil || string(b) != "from-the-sandbox\n" {
		t.Fatalf("host sees %q (%v)", b, err)
	}

	// host, read-only: readable, not writable.
	if out, _ := inSandbox(t, id, "cat /mnt/seed/seed"); out != "from-the-host" {
		t.Fatalf("read-only host volume reads %q", out)
	}

	if out, err := inSandbox(t, id, "echo x > /mnt/seed/nope"); err == nil {
		t.Fatalf("wrote to a read-only host volume: %s", out)
	}

	// pvc, created on demand: writable, and really the named docker volume.
	if out, err := inSandbox(t, id, "echo scratch-data > /mnt/scratch/f"); err != nil {
		t.Fatalf("writing to the pvc: %v %s", err, out)
	}

	if out := dockerCLI(t, "run", "--rm", "-v", freshVol+":/v", "alpine:3.20", "cat", "/v/f"); out != "scratch-data" {
		t.Fatalf("%s holds %q", freshVol, out)
	}

	// pvc with subPath: only the subdirectory is mounted.
	if out, _ := inSandbox(t, id, "cat /mnt/train/data; ls /mnt/train"); !strings.Contains(out, "in-the-subpath") ||
		strings.Contains(out, "top") {
		t.Fatalf("subPath mount shows %q", out)
	}

	if s := a.call("DELETE", "/v1/sandboxes/"+id, "", nil); s != 204 {
		t.Fatalf("delete = %d", s)
	}

	// The volume this create made goes with it; the one that already existed stays.
	if err := exec.Command("docker", "volume", "inspect", freshVol).Run(); err == nil {
		t.Errorf("%s survived deleteOnSandboxTermination", freshVol)
	}

	if err := exec.Command("docker", "volume", "inspect", seededVol).Run(); err != nil {
		t.Errorf("%s, which existed before the sandbox, was deleted with it", seededVol)
	}
}
