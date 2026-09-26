package cli

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A daemon started with --only fronts the sandboxes in its scope. `sbx create` of one of them,
// before that daemon's refresh tick has bound its ports, must be told to wait for the daemon it
// has - not to start one, which is the opposite advice and would start a second front.
func TestReadinessFindsAScopedDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	defer func(w time.Duration) { pickupWait = w }(pickupWait)
	pickupWait = 50 * time.Millisecond

	scope, _ := daemon.ParseScope([]string{"osb-"})
	defer daemon.Announce("docker", scope)()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	eps := []provider.Endpoint{{Host: "127.0.0.1", Port: port}}

	if got := readiness("osb-a", eps); strings.Contains(got, "no `sbx serve` is running") {
		t.Errorf("a sandbox inside a running daemon's --only was told no daemon runs:\n%s", got)
	}

	// Outside every scope nothing fronts it, and that advice is still the right one.
	if got := readiness("zopnight", eps); !strings.Contains(got, "no `sbx serve` is running") {
		t.Errorf("a sandbox outside the scoped daemon's --only was not told to start one:\n%s", got)
	}
}

// `sbx list`'s warning names exactly the local sandboxes no running daemon fronts.
func TestListWarnsOnlyAboutSandboxesNoDaemonServes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	scope, _ := daemon.ParseScope([]string{"osb-"})
	defer daemon.Announce("docker", scope)()

	units := []provider.Unit{
		{Sandbox: "osb-a", Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20000}}},
		{Sandbox: "osb-a", Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20001}}},
		{Sandbox: "zopnight", Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20010}}},
	}

	if got := strings.Join(unserved(units), ","); got != "zopnight" {
		t.Errorf("unserved = %q, want only the sandbox outside --only (zopnight)", got)
	}
}

// doctor reports a daemon started with --only, and says which sandboxes it fronts - it is not
// "not running", and it is not the machine's daemon either.
func TestDoctorReportsAScopedDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	scope, _ := daemon.ParseScope([]string{"osb-"})
	defer daemon.Announce("docker", scope)()

	c := daemonCapability()
	if c.Detail == "not running" || !strings.Contains(c.Detail+c.Meaning, scope.String()) {
		t.Errorf("doctor's sbx serve row = %+v, want it to report the daemon scoped to %s", c, scope)
	}
}
