package daemon

import (
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestARealContainerCallingOutStampsActivity is the use case, run rather than described: a box
// that nothing dials, doing the only visible thing an agent does - calling an API - and being
// counted as busy for it.
//
// It uses a real container and a real HTTP_PROXY, because the mechanism being checked is one a
// unit test cannot reach: whether an off-the-shelf client inside a container, configured the way
// sbx configures one, produces a stamp on the way out.
//
// The proxy sits on the host here rather than on the sandbox bridge, which is where the daemon
// puts it. That is the one seam: on this platform the daemon cannot bind the bridge gateway at
// all (see bindable), so the bridge half is covered by the Linux e2e and this covers the half
// that decides whether a working box sleeps.
func TestARealContainerCallingOutStampsActivity(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker on PATH")
	}

	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no docker daemon")
	}

	var n atomic.Int64

	f := NewEgressFilter([]string{"example.com"})
	f.OnActivity = func() { n.Add(1) }

	// 0.0.0.0, not loopback: the client is in a container and comes in over the bridge.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	defer srv.Close()

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	proxy := "http://host.docker.internal:" + port

	// http_proxy for the plain request; alpine's wget honours it. --add-host keeps this working
	// on the engines where host.docker.internal is not resolvable by default.
	cmd := exec.Command("docker", "run", "--rm",
		"--add-host", "host.docker.internal:host-gateway",
		"-e", "http_proxy="+proxy,
		"-e", "HTTP_PROXY="+proxy,
		"alpine:3.20", "wget", "-q", "-T", "20", "-O", "/dev/null", "http://example.com/")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("the container could not reach the host proxy (%v): %s", err, out)
	}

	if n.Load() == 0 {
		t.Fatal("a real container fetched an allow-listed URL through the filter and stamped " +
			"no activity: an agent box would still look idle while it worked")
	}

	t.Logf("a container's outbound fetch produced %d stamps", n.Load())
}

// TestARefusedHostFromARealContainerStampsNothing is the other half: the box that is busy
// hammering something it may not reach must not be able to hold its own memory open.
func TestARefusedHostFromARealContainerStampsNothing(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker on PATH")
	}

	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no docker daemon")
	}

	var n atomic.Int64

	f := NewEgressFilter([]string{"allowed.example"})
	f.OnActivity = func() { n.Add(1) }

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	defer srv.Close()

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	proxy := "http://host.docker.internal:" + port

	cmd := exec.Command("docker", "run", "--rm",
		"--add-host", "host.docker.internal:host-gateway",
		"-e", "http_proxy="+proxy,
		"-e", "HTTP_PROXY="+proxy,
		"alpine:3.20", "wget", "-q", "-T", "20", "-O", "/dev/null", "http://blocked.example/")

	// wget is expected to fail: the filter answers 403.
	_, _ = cmd.CombinedOutput()

	if n.Load() != 0 {
		t.Fatalf("a refused fetch stamped %d times; a box that can reach nothing would still "+
			"never sleep", n.Load())
	}
}
