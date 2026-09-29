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

	// Refused before anything is created, because the teardown below removes EVERYTHING under
	// this name, volumes included. A create over an existing sandbox does not fail - each
	// service reports "already exists" and is reused - so without this, `sbx with X` against a
	// branch sandbox X ran the command and then destroyed X and its data. Confirmed live: a
	// redis key set in X was gone after `sbx with X -- true`. It is also what makes removing a
	// half-finished create safe: whatever exists under the name afterwards, this run made.
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	if len(units) > 0 {
		return fmt.Errorf("sandbox %q already exists, and sbx with removes the sandbox it runs against\n"+
			"     when the command ends - so it only takes a name that is not in use. Pick an unused\n"+
			"     name, or run against %q without removing it:\n"+
			"       eval \"$(sbx env %s)\" && <command>\n"+
			"       sbx exec %s <service> <command>",
			sandbox, sandbox, sandbox, sandbox)
	}

	return runScoped(
		func() error { return createWithin(ctx, p, path, sandbox, withOptional, iso, timeout) },
		func() error { return Ready(ctx, p, sandbox, timeout) },
		func() ([][2]string, error) { return envVars(ctx, p, path, sandbox) },
		func(vars [][2]string) error { return runCommand(ctx, args, vars) },
		func() error {
			// context.Background(), not ctx: if the command was killed by a cancelled ctx,
			// the teardown still has to run, or the interrupt that stopped the test would
			// also leak its fixture - the exact failure this command exists to prevent.
			bg := context.Background()

			// A create that failed before its first container existed left nothing, and removing
			// nothing is an error to the provider ("no sandbox") that would bury the real one.
			if units, err := p.List(bg, sandbox); err == nil && len(units) == 0 {
				return nil
			}

			fmt.Fprintf(os.Stderr, "  removing ephemeral sandbox %q\n", sandbox)

			if err := Remove(bg, p, sandbox); err != nil {
				return fmt.Errorf("%w - remove it by hand: sbx rm %s", err, sandbox)
			}

			return nil
		},
		keep,
	)
}

// runScoped is the lifecycle, with each step injected so it is testable without docker: create,
// then guarantee teardown, then ready, env and run. Teardown fires on every path except when
// keep is set - a failed create, a failed ready, a failed command, a panic.
//
// A failed create is torn down too. It rarely fails before making anything: a health check that
// never passes leaves a running, unhealthy container, and a failed `docker run` leaves one in
// "Created". This used to return here on the grounds that nothing was created, which was false,
// and the next `sbx with` under the same name then refused it as an existing sandbox. The caller
// guarantees the name was unused before create, so everything under it is this run's to remove.
func runScoped(create, ready func() error, env func() ([][2]string, error), run func([][2]string) error, remove func() error, keep bool) (err error) {
	if e := create(); e != nil {
		if keep {
			return e
		}

		// The create's error is the one that explains what happened. A failed teardown is
		// added to it rather than replacing it, so the leftover is still mentioned.
		if re := remove(); re != nil {
			return fmt.Errorf("%w\n     and removing what it had created failed: %v", e, re)
		}

		return e
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
