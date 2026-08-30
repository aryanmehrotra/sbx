package egress

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildContextHasWhatADockerBuildNeeds(t *testing.T) {
	files, err := BuildContext("golang:1.26-alpine", "alpine:3.20")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"Dockerfile", "go.mod", "filter.go", "main.go"} {
		if files[want] == "" {
			t.Fatalf("the context has no %s", want)
		}
	}

	if !strings.HasPrefix(files["filter.go"], "package main\n") {
		t.Fatal("filter.go was not rewritten to package main; the build would fail inside the " +
			"container, where nothing reports it")
	}

	if strings.Contains(files["filter.go"], "package egress") {
		t.Fatal("a second package clause survived the rewrite")
	}

	// The pin has to reach the Dockerfile, or the image is whatever :latest happened to be.
	for _, want := range []string{"golang:1.26-alpine", "alpine:3.20"} {
		if !strings.Contains(files["Dockerfile"], want) {
			t.Fatalf("the Dockerfile does not name %s", want)
		}
	}
}

func TestBuildContextRefusesWithoutPins(t *testing.T) {
	if _, err := BuildContext("", "alpine:3.20"); err == nil {
		t.Fatal("a context with no builder image was accepted; the build would pick :latest")
	}

	if _, err := BuildContext("golang:1.26-alpine", ""); err == nil {
		t.Fatal("a context with no runtime image was accepted")
	}
}

// TestTheGeneratedContextCompilesAndFilters is the one that earns the design. The container runs
// a copy of this package's source, and the way that stays true is to build the copy and put real
// traffic through it - allowed host through, blocked host refused - rather than to assert that
// two files look alike.
//
// It compiles on the host rather than in a container: the question is whether the generated
// source is a valid program that enforces the list, and that answer does not change with the
// kernel it runs on. Whether docker can build it is the provider's test.
func TestTheGeneratedContextCompilesAndFilters(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a program; not for -short")
	}

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}

	files, err := BuildContext("golang:1.26-alpine", "alpine:3.20")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	for name, body := range files {
		if name == "Dockerfile" {
			continue
		}

		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	bin := filepath.Join(dir, "filter")

	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the generated context does not compile: %v\n%s", err, out)
	}

	// Something to be allowed to reach.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream says hello")
	}))
	defer upstream.Close()

	proxyPort, statPort := freePort(t), freePort(t)

	cmd := exec.Command(bin,
		"-allow", "127.0.0.1",
		"-listen", "127.0.0.1:"+proxyPort,
		"-stat", "127.0.0.1:"+statPort)

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	defer func() { _ = cmd.Process.Kill() }()

	waitFor(t, "127.0.0.1:"+proxyPort)

	tr := &http.Transport{Proxy: fixedProxy(t, "http://127.0.0.1:"+proxyPort)}

	// Allowed.
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != "upstream says hello" {
		t.Fatalf("the compiled filter did not carry an allowed request: %q", body)
	}

	// Refused: a host that is not on the list.
	blocked, _ := http.NewRequest(http.MethodGet, "http://blocked.example/", nil)

	bresp, err := tr.RoundTrip(blocked)
	if err != nil {
		t.Fatal(err)
	}

	defer bresp.Body.Close()

	if bresp.StatusCode != http.StatusForbidden {
		t.Fatalf("a blocked host got %d, want 403: the container copy does not enforce the list",
			bresp.StatusCode)
	}

	// And the stat endpoint the daemon scrapes moved, because traffic went through.
	sresp, err := http.Get("http://127.0.0.1:" + statPort + "/last")
	if err != nil {
		t.Fatal(err)
	}

	defer sresp.Body.Close()

	raw, _ := io.ReadAll(sresp.Body)
	if strings.TrimSpace(string(raw)) == "" {
		t.Fatal("the stat endpoint returned nothing; the daemon would never stamp this sandbox")
	}

	t.Logf("the compiled container filter allowed one host, refused another, and reports last "+
		"activity as %s", strings.TrimSpace(string(raw)))
}

func freePort(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer l.Close()

	_, port, _ := net.SplitHostPort(l.Addr().String())

	return port
}

func waitFor(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("%s never came up", addr)
}
