package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Ephemeral fixtures, the Testcontainers shape.
//
// `sbx with <sandbox> --template postgres -- go test ./...` creates the sandbox, waits until it
// serves, runs the command with the sandbox's env exported, and ALWAYS removes it afterwards -
// on success, on a failing test, or on an interrupt. The fixture lives exactly as long as the
// command and cleans up after itself, which is the one thing a plain create/env/rm script does
// not guarantee: a test that panics or a runner that is killed leaks the sandbox and its volume.
//
// It is the opposite lifecycle from the rest of sbx. A branch sandbox sleeps to 0 B and waits
// to be woken again; an ephemeral one is destroyed, because a test fixture that survives the
// test is a leak, not a saving. `--keep` overrides that for the case where a failure is worth
// inspecting.

// ChildExit carries the run command's own exit status up to main() so `sbx with -- go test`
// exits with the test's code, which is what CI gates on. It prints nothing itself: the child
// already wrote its own output. main() recognises it by the ChildStatus method, which
// deliberately is not ExitCode - so an ordinary `sbx exec` failure keeps the normal "sbx: ..."
// diagnostic and exit 1, and only a scoped run substitutes the child's status.
type ChildExit struct{ Code int }

func (e *ChildExit) Error() string    { return fmt.Sprintf("the command exited with status %d", e.Code) }
func (e *ChildExit) ChildStatus() int { return e.Code }

// With runs a command against a freshly created, always-removed sandbox.
func With(ctx context.Context, p provider.Provider, path, sandbox string, withOptional bool, iso provider.Isolation, timeout time.Duration, keep bool, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sbx with needs a command to run: sbx with <sandbox> [--template T] -- <command>")
	}

	return runScoped(
		func() error { return Create(ctx, p, path, sandbox, withOptional, iso) },
		func() error { return Ready(ctx, p, sandbox, timeout) },
		func() ([][2]string, error) { return envVars(ctx, p, path, sandbox) },
		func(vars [][2]string) error { return runCommand(ctx, args, vars) },
		func() error {
			// context.Background(), not ctx: if the command was killed by a cancelled ctx,
			// the teardown still has to run, or the interrupt that stopped the test would
			// also leak its fixture - the exact failure this command exists to prevent.
			fmt.Fprintf(os.Stderr, "  removing ephemeral sandbox %q\n", sandbox)
			return Remove(context.Background(), p, sandbox)
		},
		keep,
	)
}

// runScoped is the lifecycle, with each step injected so it is testable without docker: create,
// then guarantee teardown, then ready, env and run. Teardown fires on every path after a
// successful create except when keep is set - a failed ready, a failed command, a panic.
func runScoped(create, ready func() error, env func() ([][2]string, error), run func([][2]string) error, remove func() error, keep bool) (err error) {
	if e := create(); e != nil {
		return e // nothing was created, so nothing to tear down
	}

	torn := false

	teardown := func() {
		if keep || torn {
			return
		}

		torn = true

		if e := remove(); e != nil && err == nil {
			err = fmt.Errorf("removing the ephemeral sandbox: %w", e)
		}
	}
	defer teardown()

	if e := ready(); e != nil {
		return e
	}

	vars, e := env()
	if e != nil {
		return e
	}

	return run(vars)
}

// runCommand runs the user's command with the sandbox's variables added to the environment, and
// turns a non-zero exit into a ChildExit so the status survives to the process's own exit code.
func runCommand(ctx context.Context, args []string, vars [][2]string) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = os.Environ()

	for _, kv := range vars {
		cmd.Env = append(cmd.Env, kv[0]+"="+kv[1])
	}

	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	if e := cmd.Run(); e != nil {
		var ee *exec.ExitError
		if errors.As(e, &ee) {
			return &ChildExit{Code: ee.ExitCode()}
		}

		return fmt.Errorf("running %q: %w", args[0], e)
	}

	return nil
}
