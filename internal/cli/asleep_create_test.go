package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// existingStub is a Provider holding one service that already exists, awake or asleep. Create
// finds it and does nothing, as docker's does; Exec fails the way docker does against a stopped
// container, and counts every call.
type existingStub struct {
	provider.Provider

	fresh   bool // absent until Create makes it, as a service this create is the first to make
	created bool
	running bool
	execErr error
	execs   int
	probes  int
	stopped []string
}

func (s *existingStub) Probe(context.Context, string) (bool, bool) {
	s.probes++
	return true, true
}

func (s *existingStub) Endpoints(_, _ string, _, _ int, ports []int) []provider.Endpoint {
	eps := make([]provider.Endpoint, 0, len(ports))
	for _, p := range ports {
		eps = append(eps, provider.Endpoint{Host: "127.0.0.1", Port: 20000 + p})
	}

	return eps
}

func (s *existingStub) Create(context.Context, string, int, int, string, spec.Service,
	[]provider.Endpoint, string, provider.Isolation,
) error {
	s.created = true

	return nil
}

func (s *existingStub) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	if s.fresh && !s.created {
		return nil, nil
	}

	return []provider.Unit{{Sandbox: sandbox, Service: "clickhouse", Ref: "sbx-x-clickhouse", Running: s.running}}, nil
}

func (s *existingStub) Exec(context.Context, string, []string) (string, error) {
	s.execs++
	return "", s.execErr
}

func clickhouseWithAFile() spec.Service {
	return spec.Service{
		Ports:  []int{9000},
		Files:  map[string]string{"/host/low-mem.xml": "/etc/clickhouse-server/config.d/low-mem.xml"},
		Health: "clickhouse-client -q 'SELECT 1'",
		Init:   []string{"clickhouse-client -q 'CREATE DATABASE IF NOT EXISTS x'"},
	}
}

// Re-running `sbx create` over a sandbox that is asleep must not report a mount failure.
//
// Its containers exist and are stopped - that is what asleep is. The file check, the health
// wait and init all exec into the container, and an exec into a stopped one fails. The file
// check read that failure as "the runtime created a directory where your file should be" and
// sent the reader to move a file that had mounted perfectly well: awake, the same path was a
// regular 1726-byte file.
func TestCreateOverAnAsleepServiceDoesNotReportAMountFailure(t *testing.T) {
	stub := &existingStub{
		running: false,
		execErr: errors.New("docker exec sbx-x-clickhouse test -f /etc/x: exit status 1: " +
			"Error response from daemon: container 753ddaeb is not running"),
	}

	err := createOne(context.Background(), stub, "x", 0, 0, "clickhouse", clickhouseWithAFile(), ".", provider.IsolationContainer)
	if err != nil {
		t.Fatalf("create over an asleep service failed: %v", err)
	}

	if stub.execs != 0 || stub.probes != 0 {
		t.Errorf("touched a stopped container: %d exec(s), %d probe(s); every one of them can only fail", stub.execs, stub.probes)
	}
}

// The check still does its job on a container that is running: a file that really did arrive
// as a directory is still named.
func TestCreateStillReportsAFileThatMountedAsADirectory(t *testing.T) {
	stub := &existingStub{running: true, execErr: errors.New("exit status 1")}

	err := createOne(context.Background(), stub, "x", 0, 0, "clickhouse", clickhouseWithAFile(), ".", provider.IsolationContainer)
	if err == nil || !strings.Contains(err.Error(), "did not mount as a file") {
		t.Fatalf("a running container whose file is a directory got %v, want the mount error", err)
	}
}

// A service this create made is always checked, even when it lists as asleep straight away.
// A microVM's Create ends by putting the new VM to sleep, so "not running" alone cannot mean
// "already existed": skipping on it would leave every new microVM without its init.
func TestAFreshServiceThatListsAsAsleepStillGetsItsHealthAndInit(t *testing.T) {
	svc := clickhouseWithAFile()
	svc.Files = nil // a microVM refuses files; health and init are what is at stake

	stub := &existingStub{fresh: true, running: false}

	if err := createOne(context.Background(), stub, "x", 0, 0, "clickhouse", svc, ".", provider.IsolationContainer); err != nil {
		t.Fatalf("fresh create failed: %v", err)
	}

	if stub.probes == 0 || stub.execs == 0 {
		t.Errorf("a fresh service got %d health probe(s) and %d exec(s), want its health check and its init step", stub.probes, stub.execs)
	}
}

// Stop records rather than panicking through the nil Provider: a failed mount check stops the
// container on a backend that cannot remove one on its own.
func (s *existingStub) Stop(_ context.Context, ref string) error {
	s.stopped = append(s.stopped, ref)

	return nil
}
