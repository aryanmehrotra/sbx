package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// The guarantee `sbx with` exists for: the ephemeral sandbox is removed on every path after a
// successful create, whatever the command did - because a fixture that survives a failed or
// killed test is a leak, which is the failure a create/env/rm script has and this does not.

func TestScopedRunRemovesAfterASuccessfulCommand(t *testing.T) {
	var log []string
	err := runScoped(
		rec(&log, "create", nil), rec(&log, "ready", nil), envOK(&log),
		runRec(&log, nil), rec(&log, "remove", nil), false,
	)
	if err != nil {
		t.Fatal(err)
	}

	assertOrder(t, log, "create", "ready", "env", "run", "remove")
}

func TestScopedRunRemovesEvenWhenTheCommandFails(t *testing.T) {
	var log []string
	boom := &ChildExit{Code: 7}
	err := runScoped(
		rec(&log, "create", nil), rec(&log, "ready", nil), envOK(&log),
		runRec(&log, boom), rec(&log, "remove", nil), false,
	)

	var ce *ChildExit
	if !errors.As(err, &ce) || ce.Code != 7 {
		t.Fatalf("want the command's ChildExit{7} to propagate, got %v", err)
	}

	if !contains(log, "remove") {
		t.Errorf("the sandbox was not removed after a failing command: %v", log)
	}
}

func TestScopedRunRemovesWhenReadyNeverServes(t *testing.T) {
	var log []string
	err := runScoped(
		rec(&log, "create", nil), rec(&log, "ready", errors.New("never became ready")),
		envOK(&log), runRec(&log, nil), rec(&log, "remove", nil), false,
	)
	if err == nil {
		t.Fatal("expected the ready failure to surface")
	}

	if contains(log, "run") {
		t.Errorf("the command ran despite ready failing: %v", log)
	}

	if !contains(log, "remove") {
		t.Errorf("a sandbox that never served was left behind: %v", log)
	}
}

// A create that fails partway has usually made something: a container whose health never
// passed, or one docker left in "Created" after `docker run` failed. `sbx with` only ever
// creates a sandbox it just checked did not exist, so everything under that name is its own and
// is removed - the create's error is what surfaces, not the teardown's.
func TestScopedRunRemovesWhenCreateFails(t *testing.T) {
	var log []string
	createErr := errors.New(`service "redis": never became ready within 20s`)

	err := runScoped(
		rec(&log, "create", createErr), rec(&log, "ready", nil),
		envOK(&log), runRec(&log, nil), rec(&log, "remove", nil), false,
	)
	if !errors.Is(err, createErr) {
		t.Fatalf("want the create failure to surface, got %v", err)
	}

	assertOrder(t, log, "create", "remove")
}

// If the teardown after a failed create also fails, the create's error is still the one that
// explains what happened, and the leftover is named so it can be removed by hand.
func TestScopedRunKeepsTheCreateErrorWhenTeardownAlsoFails(t *testing.T) {
	var log []string
	createErr := errors.New("docker run failed")

	err := runScoped(
		rec(&log, "create", createErr), rec(&log, "ready", nil),
		envOK(&log), runRec(&log, nil), rec(&log, "remove", errors.New("daemon busy")), false,
	)
	if !errors.Is(err, createErr) {
		t.Fatalf("the create failure was replaced: %v", err)
	}

	if !strings.Contains(err.Error(), "daemon busy") {
		t.Errorf("the failed teardown was not reported, so a leftover goes unmentioned: %v", err)
	}
}

func TestScopedRunKeepLeavesAFailedCreate(t *testing.T) {
	var log []string
	_ = runScoped(
		rec(&log, "create", errors.New("never became ready")), rec(&log, "ready", nil),
		envOK(&log), runRec(&log, nil), rec(&log, "remove", nil), true, // keep
	)

	if contains(log, "remove") {
		t.Errorf("--keep asks to inspect a failure, but remove ran: %v", log)
	}
}

// withStub is a Provider that records the lifecycle calls `sbx with` makes. It holds the units
// of an existing sandbox when given some, and otherwise creates a redis whose health check
// never passes.
type withStub struct {
	provider.Provider

	existing []provider.Unit
	created  bool
	removed  bool
}

func (s *withStub) Name() string { return "stub" }

func (s *withStub) AllocSlot(context.Context, string) (int, error) { return 0, nil }

func (s *withStub) Endpoints(_, _ string, _, _ int, ports []int) []provider.Endpoint {
	eps := make([]provider.Endpoint, 0, len(ports))
	for _, p := range ports {
		eps = append(eps, provider.Endpoint{Host: "127.0.0.1", Port: 20000 + p})
	}

	return eps
}

func (s *withStub) Create(context.Context, string, int, int, string, spec.Service,
	[]provider.Endpoint, string, provider.Isolation,
) error {
	s.created = true

	return nil
}

func (s *withStub) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	if s.existing != nil {
		return s.existing, nil
	}

	if s.created && !s.removed {
		return []provider.Unit{{Sandbox: sandbox, Service: "redis", Ref: "sbx-" + sandbox + "-redis", Running: true}}, nil
	}

	return nil, nil
}

func (s *withStub) Probe(context.Context, string) (bool, bool) { return false, true }

func (s *withStub) Exec(context.Context, string, []string) (string, error) {
	return "", errors.New("exit status 1")
}

func (s *withStub) Logs(context.Context, string, int, bool, io.Writer) error { return nil }

func (s *withStub) Remove(context.Context, string) error {
	s.removed = true

	return nil
}

func redisSpec(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sandbox.json")
	body := `{"version": 1, "services": {"redis": {"image": "redis:7-alpine", "ports": [6379], "health": "redis-cli ping"}}}`

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

// `sbx with X` against a sandbox X that already exists used to reuse its services ("already
// exists"), run the command, and then remove X - volumes and all - as if it were the ephemeral
// fixture. Confirmed live: a redis key set in X was gone after `sbx with X -- true`. The name
// has to be refused before anything is created or removed.
func TestWithRefusesASandboxThatAlreadyExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := &withStub{existing: []provider.Unit{{Sandbox: "mine", Service: "redis", Ref: "sbx-mine-redis", Running: true}}}

	err := With(context.Background(), p, redisSpec(t), "mine", false, provider.IsolationContainer,
		time.Second, false, []string{"true"})
	if err == nil {
		t.Fatal("sbx with ran against a sandbox that already exists")
	}

	if p.created || p.removed {
		t.Errorf("an existing sandbox was touched: created=%v removed=%v", p.created, p.removed)
	}

	for _, want := range []string{`"mine"`, "already exists", "sbx env mine"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// --timeout is the whole budget for services to serve. The create's own health wait was a fixed
// two minutes, so `sbx with --timeout 20s` against a redis that never answered waited 2m and
// then left the unhealthy sandbox behind.
func TestWithHonoursItsTimeoutAndRemovesAFailedCreate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := &withStub{}
	began := time.Now()

	err := With(context.Background(), p, redisSpec(t), "fx-with-timeout", false,
		provider.IsolationContainer, 300*time.Millisecond, false, []string{"true"})
	if err == nil {
		t.Fatal("a redis that never served was reported as ready")
	}

	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("--timeout 300ms waited %s: the create's health wait ignored it", took)
	}

	if !strings.Contains(err.Error(), "300ms") {
		t.Errorf("the error does not name the timeout that was used: %v", err)
	}

	if !p.removed {
		t.Error("a create that failed partway was left behind")
	}
}

func TestScopedRunKeepLeavesTheSandbox(t *testing.T) {
	var log []string
	if err := runScoped(
		rec(&log, "create", nil), rec(&log, "ready", nil), envOK(&log),
		runRec(&log, nil), rec(&log, "remove", nil), true, // keep
	); err != nil {
		t.Fatal(err)
	}

	if contains(log, "remove") {
		t.Errorf("--keep should leave the sandbox, but remove ran: %v", log)
	}
}

// runCommand really executes, so the exit-code path is tested against real processes rather
// than mocked - that mapping is the CI-facing contract and mocking it would test nothing.

func TestRunCommandPropagatesTheExitStatus(t *testing.T) {
	err := runCommand(context.Background(), []string{"sh", "-c", "exit 7"}, nil)

	var ce *ChildExit
	if !errors.As(err, &ce) {
		t.Fatalf("want a ChildExit, got %T: %v", err, err)
	}

	if ce.Code != 7 {
		t.Errorf("exit status not propagated: want 7, got %d", ce.Code)
	}
}

func TestRunCommandSucceedsAndExportsTheVars(t *testing.T) {
	// The env is the point: the command must see the sandbox's exports.
	err := runCommand(context.Background(),
		[]string{"sh", "-c", `[ "$DATABASE_PORT" = "20002" ] || exit 3`},
		[][2]string{{"DATABASE_PORT", "20002"}},
	)
	if err != nil {
		t.Fatalf("the command did not see DATABASE_PORT from the sandbox env: %v", err)
	}
}

func TestRunCommandReportsACommandThatCannotStart(t *testing.T) {
	err := runCommand(context.Background(), []string{"sbx-no-such-binary-xyz"}, nil)

	var ce *ChildExit
	if errors.As(err, &ce) {
		t.Fatal("a binary that cannot start is not a ChildExit; it is a real error")
	}

	if err == nil || !strings.Contains(err.Error(), "sbx-no-such-binary-xyz") {
		t.Errorf("want an error naming the missing command, got %v", err)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func rec(log *[]string, name string, err error) func() error {
	return func() error { *log = append(*log, name); return err }
}

func runRec(log *[]string, err error) func([][2]string) error {
	return func([][2]string) error { *log = append(*log, "run"); return err }
}

func envOK(log *[]string) func() ([][2]string, error) {
	return func() ([][2]string, error) { *log = append(*log, "env"); return nil, nil }
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}

	return false
}

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("lifecycle ran %v, want %v", got, want)
	}
}
