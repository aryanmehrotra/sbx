package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// exitsAfterCreate is a service whose container is still listed running when Create returns and
// when create first looks again - as docker lists one that exits at once - and has exited a moment
// later, with the runtime's record of why.
type exitsAfterCreate struct {
	existingStub

	lists int
	exit  *provider.ExitState // nil: the provider cannot say (no ExitReporter answer)
}

func (s *exitsAfterCreate) List(ctx context.Context, sandbox string) ([]provider.Unit, error) {
	if !s.created {
		return nil, nil
	}

	s.lists++

	return []provider.Unit{{Sandbox: sandbox, Service: "web", Ref: "sbx-x-web", Running: s.lists <= 2}}, nil
}

func (s *exitsAfterCreate) ExitOf(context.Context, string) (provider.ExitState, error) {
	return *s.exit, nil
}

// With no health check, a container that exited at once still printed "web ✓", and the failure
// only surfaced in the final readiness check as "could not ask its container". The line for a
// service must not claim it is up when its container is not running.
func TestCreateDoesNotTickAServiceWhoseContainerExited(t *testing.T) {
	p := &exitsAfterCreate{exit: &provider.ExitState{Status: "exited", ExitCode: 3}}

	var err error

	out := captureOutput(t, func() {
		err = createOne(context.Background(), p, "x", 0, 0, "web", spec.Service{Ports: []int{8080}}, ".",
			provider.IsolationContainer)
	})

	if strings.Contains(out, "✓") {
		t.Errorf("printed a tick for a container that exited:\n%s", out)
	}

	if err == nil {
		t.Fatalf("create of a service whose container exited succeeded; output:\n%s", out)
	}

	for _, want := range []string{`service "web"`, "not running", "exit code 3", "sbx logs x web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
}

// A provider with no record of why (a microVM's create ends with it asleep, and nothing exited)
// keeps the tick: not running there is not a failure.
func TestCreateStillTicksAServiceAsleepByDesign(t *testing.T) {
	p := &asleepAfterCreate{}

	var err error

	out := captureOutput(t, func() {
		err = createOne(context.Background(), p, "x", 0, 0, "web", spec.Service{Ports: []int{8080}}, ".",
			provider.IsolationContainer)
	})

	if err != nil || !strings.Contains(out, "✓") {
		t.Fatalf("a service asleep after create was refused: err %v, output:\n%s", err, out)
	}
}

// asleepAfterCreate lists the new service as not running from the start, and is no ExitReporter.
type asleepAfterCreate struct{ existingStub }

func (s *asleepAfterCreate) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	if !s.created {
		return nil, nil
	}

	return []provider.Unit{{Sandbox: sandbox, Service: "web", Ref: "sbx-x-web"}}, nil
}

// exitedUnits lists fixed units and reports each not-running one as exited with code 3.
type exitedUnits struct {
	unitsStub

	started time.Time
}

func (s *exitedUnits) ExitOf(context.Context, string) (provider.ExitState, error) {
	return provider.ExitState{Status: "exited", ExitCode: 3, StartedAt: s.started}, nil
}

// A container that outlived the tick and exited before the final check was skipped by it - the
// check asks only running services, so a microVM asleep after create is not failed - and create
// printed "ready" for a sandbox whose only service had exited. Where the runtime says it exited,
// the final check names it.
func TestCreateFinalCheckNamesAServiceThatExited(t *testing.T) {
	began := time.Now()

	p := &exitedUnits{unitsStub: unitsStub{units: []provider.Unit{
		{Sandbox: "x", Service: "web", Ref: "sbx-x-web", Running: false},
	}}, started: began.Add(time.Second)}

	err := checkCreatedWorkloads(context.Background(), p, "x", []string{"web"}, began, time.Now().Add(300*time.Millisecond))
	if err == nil {
		t.Fatal("create's final check passed a service whose container exited")
	}

	for _, want := range []string{"web", "not running", "exit code 3", "sbx logs x web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
}

// A re-run of create over a sandbox that is asleep keeps its services as they are, and lists them
// as made: their containers last started before this create, so being stopped is not a failure.
func TestCreateFinalCheckPassesAServiceThatWasAlreadyAsleep(t *testing.T) {
	began := time.Now()

	p := &exitedUnits{unitsStub: unitsStub{units: []provider.Unit{
		{Sandbox: "x", Service: "web", Ref: "sbx-x-web", Running: false},
	}}, started: began.Add(-time.Hour)}

	if err := checkCreatedWorkloads(context.Background(), p, "x", []string{"web"}, began, time.Now().Add(300*time.Millisecond)); err != nil {
		t.Fatalf("create's final check failed a service asleep since before this create: %v", err)
	}
}
