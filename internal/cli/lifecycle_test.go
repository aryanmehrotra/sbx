package cli

import (
	"context"
	"errors"

	"net"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// ── a create whose mount check fails ─────────────────────────────────────────

// unitRemover is existingStub on a backend that can remove one service's container.
type unitRemover struct {
	*existingStub

	removed []string
}

func (u *unitRemover) RemoveUnit(_ context.Context, ref string) error {
	u.removed = append(u.removed, ref)

	return nil
}

// A container whose mount check failed is broken by construction: every start mounts the same
// wrong path. `sbx create` used to return the error and leave it up and awake, serving with the
// broken mount - and a re-run after fixing the path found it "already exists" and kept it. The
// rest of the sandbox stays, so re-running create finishes it; this one container goes.
func TestAFailedMountCheckRemovesThatServicesContainer(t *testing.T) {
	stub := &unitRemover{existingStub: &existingStub{fresh: true, running: true, execErr: errors.New("exit status 1")}}

	err := createOne(context.Background(), stub, "x", 0, 0, "clickhouse", clickhouseWithAFile(), ".", provider.IsolationContainer)
	if err == nil || !strings.Contains(err.Error(), "did not mount as a file") {
		t.Fatalf("want the mount error, got %v", err)
	}

	if len(stub.removed) != 1 || stub.removed[0] != "sbx-x-clickhouse" {
		t.Errorf("the container with the broken mount was left up: removed %v", stub.removed)
	}

	for _, want := range []string{"removed sbx-x-clickhouse", "re-run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
}

// A backend that cannot remove one container at a time stops it instead, so it is at least not
// left serving, and the error says which of the two happened.
func TestAFailedMountCheckStopsTheContainerWhenItCannotBeRemoved(t *testing.T) {
	stub := &existingStub{fresh: true, running: true, execErr: errors.New("exit status 1")}

	err := createOne(context.Background(), stub, "x", 0, 0, "clickhouse", clickhouseWithAFile(), ".", provider.IsolationContainer)
	if err == nil {
		t.Fatal("a failed mount check was reported as success")
	}

	if len(stub.stopped) != 1 {
		t.Errorf("the container with the broken mount was left awake: stopped %v", stub.stopped)
	}

	if !strings.Contains(err.Error(), "stopped sbx-x-clickhouse") {
		t.Errorf("the error does not say the container was stopped: %v", err)
	}
}

// ── ready on a container that is not running ─────────────────────────────────

// exitedStub is one local service whose container has exited, behind a daemon port that
// accepts - the shape of the live report: `sbx serve` was up, the container was not, and
// `sbx ready` said "serving".
type exitedStub struct {
	provider.Provider

	port    int
	exitErr error
}

func (s *exitedStub) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	return []provider.Unit{{
		Sandbox: sandbox, Service: "web", Ref: "sbx-x-web", Running: false,
		Client: []provider.Endpoint{{Host: "127.0.0.1", Port: s.port}},
	}}, nil
}

// No health check declared, so nothing but the container's state can say it is not serving.
func (s *exitedStub) Healthy(context.Context, string) (bool, bool) { return false, false }
func (s *exitedStub) Probe(context.Context, string) (bool, bool)   { return false, false }

func (s *exitedStub) ExitOf(context.Context, string) (provider.ExitState, error) {
	return provider.ExitState{Status: "exited", ExitCode: 1}, s.exitErr
}

func listening(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			_ = c.Close()
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port
}

func TestReadyDoesNotCallAnExitedContainerServing(t *testing.T) {
	p := &exitedStub{port: listening(t)}

	err := Ready(context.Background(), p, "x", 300*time.Millisecond)
	if err == nil {
		t.Fatal("sbx ready reported serving for a container that is not running")
	}

	for _, want := range []string{"web", "not running", "state exited", "sbx logs x web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
}

// ── what a health wait reports when it gives up ──────────────────────────────

// probeStub never serves, and says through ExitOf why.
type probeStub struct {
	logStub

	state   provider.ExitState
	exitErr error
}

func (s *probeStub) ExitOf(context.Context, string) (provider.ExitState, error) {
	return s.state, s.exitErr
}

// A health wait that gives up on a container that is not running names its state: that is the
// answer, and the health command's own failure is only a symptom of it.
func TestAHealthWaitOnAnExitedContainerSaysItExited(t *testing.T) {
	p := &probeStub{state: provider.ExitState{Status: "exited", ExitCode: 137}}

	err := waitHealthy(context.Background(), p, "sbx-x-redis", "", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "state exited, exit code 137") {
		t.Errorf("want the container's state in the error, got %v", err)
	}
}

// And when the runtime could not be asked at all, that is the last thing known and is said, not
// swallowed: "never became ready" alone sends the reader to the workload when it was the engine.
func TestAHealthWaitThatCouldNotAskTheRuntimeSaysSo(t *testing.T) {
	p := &probeStub{exitErr: errors.New("docker GET /containers/sbx-x-redis/json: context deadline exceeded")}

	err := waitHealthy(context.Background(), p, "sbx-x-redis", "", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("want the runtime's own error in the message, got %v", err)
	}
}

// flappingStub is a container that exits on startup: the wake starts it, the first look finds it
// running, and it is gone a moment later. Found live: `sbx ready` against redis with a bad flag
// and no health check caught it in that moment and said "serving".
type flappingStub struct {
	exitedStub

	lists int
}

func (s *flappingStub) List(ctx context.Context, sandbox string) ([]provider.Unit, error) {
	s.lists++

	units, err := s.exitedStub.List(ctx, sandbox)
	units[0].Running = s.lists == 1

	return units, err
}

func TestWaitRunningDoesNotPassAContainerThatExitsOnStartup(t *testing.T) {
	p := &flappingStub{exitedStub: exitedStub{port: listening(t)}}

	err := waitRunning(context.Background(), p, "x", time.Now().Add(300*time.Millisecond), 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("a container that was running for a moment and then exited passed: %v", err)
	}
}

// cyclingStub is the same container behind a running daemon, which keeps waking it: the unit
// believed awake fails its next dial, the belief is revoked, and the container is started again.
// It is running at most looks, and two looks a settle apart can both land on one - measured live,
// one `sbx ready` in three said "serving" that way. Only a window of continuous running counts.
type cyclingStub struct {
	exitedStub

	lists int
}

func (s *cyclingStub) List(ctx context.Context, sandbox string) ([]provider.Unit, error) {
	s.lists++

	units, err := s.exitedStub.List(ctx, sandbox)
	units[0].Running = s.lists%3 != 0

	return units, err
}

func TestWaitRunningDoesNotPassAContainerTheDaemonKeepsRestarting(t *testing.T) {
	p := &cyclingStub{exitedStub: exitedStub{port: listening(t)}}

	err := waitRunning(context.Background(), p, "x", time.Now().Add(time.Second), 250*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("a container that keeps exiting passed because two looks both caught it up: %v", err)
	}
}
