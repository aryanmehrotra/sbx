package daemon

// Docker-backed: the OpenSandbox API and a scoped daemon against a real engine.
//
// These run beside whatever else is on the engine - on a developer's machine that is their live
// sandboxes and their own `sbx serve`. So the daemon here is always scoped (--only osb-, or a
// prefix owned by the test), the only containers created are ones named by this test, and the
// only ones removed are those. Nothing here stops, pauses or removes anything it did not make.

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
	"github.com/aryanmehrotra/sbx/internal/provider"
)

func dockerOrSkip(t *testing.T) provider.Provider {
	t.Helper()

	if testing.Short() {
		t.Skip("starts real containers; run without -short")
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker CLI")
	}

	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker is not reachable: %s", out)
	}

	p, err := provider.For("docker", "", "")
	if err != nil {
		t.Skipf("docker provider: %v", err)
	}

	return p
}

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()

	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}

	return strings.TrimSpace(string(out))
}

func containerState(name string) string {
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", name).CombinedOutput()
	if err != nil {
		return "absent"
	}

	return strings.TrimSpace(string(out))
}

// A scoped daemon beside somebody else's sandbox: the other sandbox is running, idle, and far
// past this daemon's idle window - and must come out exactly as it went in.
func TestDockerScopedDaemonLeavesOtherSandboxesAlone(t *testing.T) {
	p := dockerOrSkip(t)
	log.SetOutput(io.Discard)

	suffix := fmt.Sprintf("%x", time.Now().UnixNano())[:10]
	other := "scopeguard-" + suffix
	name := "sbx-" + other + "-db"

	// A sandbox as sbx labels one - but made by hand, and outside the scope below. The port
	// label points at a free port nothing listens on, so no real service is ever involved.
	public, backing := freePort(t), freePort(t)

	dockerCLI(t, "run", "-d", "--name", name,
		"--label", "sbx.sandbox="+other, "--label", "sbx.slot=59", "--label", "sbx.service=db",
		"--label", fmt.Sprintf("sbx.ports=%d:%d", public, backing),
		"alpine:3.20", "sleep", "300")

	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := New(p, time.Second, 10*time.Second, 300*time.Millisecond)
	d.scope = Scope{"osb-" + suffix}

	done := make(chan struct{})

	go func() {
		d.run(ctx)
		close(done)
	}()

	// Several discovery passes and several reaper ticks, each well past a 1 s idle window.
	time.Sleep(4 * time.Second)
	cancel()
	<-done

	if st := containerState(name); st != "running" {
		t.Fatalf("the out-of-scope sandbox is %q after a scoped daemon ran beside it; want running", st)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for ref := range d.units {
		if strings.Contains(ref, other) {
			t.Fatalf("the scoped daemon adopted %s", ref)
		}
	}
}

// The whole lifecycle through the API against real docker, with the real sbx binary built for
// linux as the agent (the stub execd until internal/execd lands).
func TestDockerOpenSandboxLifecycle(t *testing.T) {
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
	defer cancel()

	// A short idle window, so the freeze-on-idle path is exercised too.
	//
	// Scoped to ids only this test mints: another API sandbox on the same engine - a
	// conformance run, somebody's agent - must not be frozen by this daemon's 3 s idle.
	prefix := fmt.Sprintf("osb-%06x", time.Now().UnixNano()&0xffffff)

	d := New(p, 3*time.Second, 60*time.Second, time.Second)
	d.scope = Scope{prefix}

	go d.run(ctx)

	api, err := osb.New(osb.Options{Provider: p, Runtime: d, StateDir: filepath.Join(tmp, "osb"),
		Version: "dev-test", ReadyTimeout: 90 * time.Second,
		NewID: func() string { return fmt.Sprintf("%s%06x", prefix, time.Now().UnixNano()&0xffffff) }})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	defer api.Close()

	call := func(method, path, body string, out any) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}

		return resp.StatusCode
	}

	var created struct {
		ID     string `json:"id"`
		Status struct{ State, Reason, Message string }
	}

	status := call("POST", "/v1/sandboxes", `{"image":{"uri":"alpine:3.20"},"entrypoint":["sleep","3600"],
		"timeout":300,"resourceLimits":{"cpu":"500m","memory":"128Mi"},"metadata":{"suite":"sbx-docker-test"}}`, &created)
	if status != 202 {
		t.Fatalf("create = %d", status)
	}

	id := created.ID
	container := "sbx-" + id + "-sandbox"
	volume := ""

	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", container).Run()
		_ = exec.Command("docker", "volume", "rm", "sbx-"+id+"-sandbox-data").Run()

		if volume != "" {
			_ = exec.Command("docker", "volume", "rm", volume).Run()
		}
	})

	waitFor := func(want string, within time.Duration) {
		t.Helper()

		deadline := time.Now().Add(within)

		for {
			var sb struct {
				Status struct{ State, Reason, Message string }
			}
			call("GET", "/v1/sandboxes/"+id, "", &sb)

			if sb.Status.State == want {
				return
			}

			if sb.Status.State == "Failed" || time.Now().After(deadline) {
				t.Fatalf("%s is %s (%s: %s), want %s", id, sb.Status.State, sb.Status.Reason, sb.Status.Message, want)
			}

			time.Sleep(200 * time.Millisecond)
		}
	}

	waitFor("Running", 120*time.Second)

	// Running only says execd answered on the wake port - not that THIS daemon owns it. Another
	// listener on that port (another engine's daemon) answers too, and then nothing below is
	// testing the daemon under test. Say so here rather than as a freeze timeout.
	adopted := time.Now().Add(10 * time.Second)
	for {
		d.mu.Lock()
		_, ok := d.units[container]
		d.mu.Unlock()

		if ok {
			break
		}

		if time.Now().After(adopted) {
			t.Fatalf("the daemon under test never kept %s: its wake port is held by another listener", container)
		}

		time.Sleep(100 * time.Millisecond)
	}

	for _, m := range strings.Split(dockerCLI(t, "inspect", "--format",
		"{{range .Mounts}}{{.Name}}={{.Destination}}:{{.RW}} {{end}}", container), " ") {
		if strings.HasSuffix(m, "=/opt/sbx:false") {
			volume = strings.TrimSuffix(m, "=/opt/sbx:false")
		}
	}

	if !strings.HasPrefix(volume, "sbx-execd-dev-test-") {
		t.Fatalf("execd is not mounted read-only at /opt/sbx from a content-keyed volume: %q", volume)
	}

	var ep struct {
		Endpoint string
		Headers  map[string]string
	}

	if call("GET", "/v1/sandboxes/"+id+"/endpoints/44772", "", &ep) != 200 || ep.Headers["X-EXECD-ACCESS-TOKEN"] == "" {
		t.Fatalf("execd endpoint %+v", ep)
	}

	ping := func() error {
		// Generous: on a loaded VM-backed docker one Engine API call has been measured in
		// seconds, and a thaw is one. The assertion is that it answers, not how fast.
		c := http.Client{Timeout: 30 * time.Second}

		resp, err := c.Get("http://" + ep.Endpoint + "/ping")
		if err != nil {
			return err
		}

		_ = resp.Body.Close()

		if resp.StatusCode != 200 {
			return fmt.Errorf("ping %s", resp.Status)
		}

		return nil
	}

	if err := ping(); err != nil {
		t.Fatalf("execd through the wake port: %v", err)
	}

	// Idle: frozen, not stopped - and the next request thaws it.
	// Freezing waits for docker's own health check to have passed once (a sandbox is not idle
	// before it has served), and that check runs on a 5 s interval that stretches under load.
	deadline := time.Now().Add(90 * time.Second)
	for containerState(container) != "paused" {
		if time.Now().After(deadline) {
			d.mu.Lock()
			var seen []string
			for _, u := range d.units {
				u.mu.Lock()
				seen = append(seen, fmt.Sprintf("%s awake=%v frozen=%v served=%v freeze=%v idle=%s",
					u.ref, u.awake, u.frozen, u.served, u.freezeOnIdle, u.idleFor().Round(time.Millisecond)))
				u.mu.Unlock()
			}
			d.mu.Unlock()

			health, _ := exec.Command("docker", "inspect", "--format", "{{json .State.Health}}", container).CombinedOutput()

			t.Fatalf("an idle API sandbox was not frozen; it is %s\nunits: %v\nhealth: %s",
				containerState(container), seen, health)
		}

		time.Sleep(250 * time.Millisecond)
	}

	if err := ping(); err != nil {
		t.Fatalf("a frozen sandbox did not thaw on request: %v", err)
	}

	if st := containerState(container); st != "running" {
		t.Fatalf("after the thawing request the container is %s", st)
	}

	// Paused on purpose: frozen, and traffic does NOT thaw it.
	if s := call("POST", "/v1/sandboxes/"+id+"/pause", "", nil); s != 202 {
		t.Fatalf("pause = %d", s)
	}

	waitFor("Paused", 10*time.Second)

	if st := containerState(container); st != "paused" {
		t.Fatalf("paused through the API, container is %s", st)
	}

	if err := ping(); err == nil {
		t.Fatal("a request reached a sandbox paused on purpose")
	}

	if st := containerState(container); st != "paused" {
		t.Fatalf("a request thawed a sandbox paused on purpose: %s", st)
	}

	if s := call("POST", "/v1/sandboxes/"+id+"/resume", "", nil); s != 202 {
		t.Fatalf("resume = %d", s)
	}

	waitFor("Running", 10*time.Second)

	if err := ping(); err != nil {
		t.Fatalf("after resume: %v", err)
	}

	if s := call("DELETE", "/v1/sandboxes/"+id, "", nil); s != 204 {
		t.Fatalf("delete = %d", s)
	}

	if st := containerState(container); st != "absent" {
		t.Fatalf("after delete the container is %s", st)
	}
}
