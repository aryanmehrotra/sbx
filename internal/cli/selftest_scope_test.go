package cli

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// lister hands the daemon a fixed set of units and nothing else.
type lister struct {
	provider.Provider
	units []provider.Unit
}

func (l *lister) Name() string                                          { return "fake" }
func (l *lister) List(context.Context, string) ([]provider.Unit, error) { return l.units, nil }

// bound dials the fronted ports, and a connection to a unit wakes it: these make that wake a
// harmless no-op on a unit that already reports serving.
func (l *lister) Start(context.Context, string) error                    { return nil }
func (l *lister) Probe(context.Context, string) (serving, declared bool) { return true, true }

func freeTCPPort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	return ln.Addr().(*net.TCPAddr).Port
}

// bound reports whether something accepts on the port. A dial, not a listen: probing by binding
// the port itself could win it for a moment while the daemon under test was binding, and fail
// the very bind the test is waiting on.
func bound(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
	if err != nil {
		return false
	}

	_ = c.Close()

	return true
}

// selftest runs a real daemon in-process beside the user's own `sbx serve`. Unscoped, its 1s
// discovery adopted every sandbox on the engine: it raced the real daemon for their ports and
// its reaper slept a live stack it had no business touching. It may adopt its own sandbox and
// nothing else - not a stranger, and not another selftest whose name it is a prefix of.
//
// The logger is left alone here: swapping it back while the daemon's listener goroutines are still
// logging is a data race under -race. Capture is TestSelftestDaemonLogStaysOffStdout's subject.
func TestSelftestDaemonAdoptsOnlyItsOwnSandbox(t *testing.T) {
	mine, theirs, sibling := freeTCPPort(t), freeTCPPort(t), freeTCPPort(t)
	up := []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}

	p := &lister{units: []provider.Unit{
		{Ref: "sbx-selftest-42-redis", Sandbox: "selftest-42", Service: "redis", Running: true,
			Listen: []int{mine}, Upstream: up},
		{Ref: "sbx-zopnight-billing", Sandbox: "zopnight", Service: "billing", Running: true,
			Listen: []int{theirs}, Upstream: up},
		{Ref: "sbx-selftest-421-redis", Sandbox: "selftest-421", Service: "redis", Running: true,
			Listen: []int{sibling}, Upstream: up},
	}}

	// Stop the daemon and wait for its listener to close before the test returns. Left running,
	// its goroutines went on logging into the next test's logger swap - a data race under -race -
	// and held a port a later run could be handed.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()

		for deadline := time.Now().Add(3 * time.Second); bound(mine) && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	})

	d := selftestDaemon(p, "selftest-42")
	d.Refresh(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !bound(mine) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if !bound(mine) {
		t.Fatal("the selftest daemon did not front its own sandbox")
	}

	if bound(theirs) {
		t.Fatal("the selftest daemon bound another sandbox's port: it would front and sleep it")
	}

	if bound(sibling) {
		t.Fatal("the selftest daemon adopted selftest-421 because its own name is a prefix of it")
	}
}

// The daemon logs through logs.Default, which is stdout. In selftest that interleaved raw JSON
// lines with the step table. While selftest runs they go to a buffer instead.
func TestSelftestDaemonLogStaysOffStdout(t *testing.T) {
	restore, buf := quietDaemonLog()

	logs.Default.Info("selftest-42", "redis", "woke")

	restore()

	if !strings.Contains(buf.String(), "woke") {
		t.Fatalf("the daemon's line did not reach the capture buffer: %q", buf.String())
	}
}
