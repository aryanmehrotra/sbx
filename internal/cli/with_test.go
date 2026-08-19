package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
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

func TestScopedRunDoesNotRemoveWhenCreateFails(t *testing.T) {
	var log []string
	err := runScoped(
		rec(&log, "create", errors.New("out of slots")), rec(&log, "ready", nil),
		envOK(&log), runRec(&log, nil), rec(&log, "remove", nil), false,
	)
	if err == nil {
		t.Fatal("expected the create failure to surface")
	}

	if contains(log, "remove") {
		t.Errorf("remove ran though nothing was created: %v", log)
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
