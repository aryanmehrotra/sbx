package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/slotlock"
)

// Ephemeral fixtures, the Testcontainers shape.
//
// `sbx with <sandbox> --template postgres -- go test ./...` creates the sandbox, waits until it
// serves, runs the command with the sandbox's env exported, and ALWAYS removes it afterwards -
// on success, on a failing test, or on an interrupt (SIGINT or SIGTERM, which are passed on to
// the command first). The fixture lives exactly as long as the command and cleans up after
// itself, which is the one thing a plain create/env/rm script does not guarantee: a test that
// panics or a runner that is killed leaks the sandbox and its volume.
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

// Interrupted is a signal that stopped `sbx with`. It is the cancelled context's cause, so the
// command is sent the same signal rather than a kill, and it exits 128+signal - 130 for Ctrl-C,
// 143 for SIGTERM - which is what a shell or a CI runner reads as "stopped by that signal".
type Interrupted struct{ Signal os.Signal }

// Error names the signal as a shell does: os.Signal's own String says "interrupt" for SIGINT.
func (e *Interrupted) Error() string {
	switch e.Signal {
	case syscall.SIGINT:
		return "interrupted by SIGINT"
	case syscall.SIGTERM:
		return "interrupted by SIGTERM"
	}

	return fmt.Sprintf("interrupted by %v", e.Signal)
}

func (e *Interrupted) ChildStatus() int {
	if s, ok := e.Signal.(syscall.Signal); ok {
		return 128 + int(s)
	}

	return 130
}

// SignalContext is a context cancelled by the first SIGINT or SIGTERM, with an *Interrupted
// naming it as the cause.
//
// `sbx with` had no handler at all, so either signal killed it on the spot - rc 130 or 143 and
// the sandbox left behind, which is the leak this command exists to prevent. With the handler
// the process survives the signal long enough to stop the command and remove the sandbox. A
// second signal says what is still happening; a third is let through, for someone who would
// rather leave the sandbox than wait.
func SignalContext(parent context.Context) (context.Context, func()) {
	return signalContext(parent, "sbx: still removing the sandbox - interrupt once more to leave it behind")
}

// CreateSignalContext is SignalContext for `sbx create`, which after an interrupt is only finishing
// the step it is in and letting go of its locks - so a second signal says that, not "removing".
func CreateSignalContext(parent context.Context) (context.Context, func()) {
	return signalContext(parent, "sbx: still stopping the create - interrupt once more to exit now, leaving its name lock")
}

func signalContext(parent context.Context, still string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)

	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})

	go func() {
		select {
		case s := <-ch:
			cancel(&Interrupted{Signal: s})
		case <-done:
			return
		}

		select {
		case <-ch:
			fmt.Fprintln(os.Stderr, still)
			signal.Stop(ch)
		case <-done:
		}
	}()

	var once sync.Once

	return ctx, func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
			cancel(nil)
		})
	}
}

// teardownBudget bounds the removal after the command. Its own context, not the command's: an
// interrupt cancels that one, and the removal is what has to happen after an interrupt.
const teardownBudget = 2 * time.Minute

// With runs a command against a freshly created, always-removed sandbox.
func With(ctx context.Context, p provider.Provider, path, sandbox string, withOptional bool, iso provider.Isolation, timeout time.Duration, keep bool, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sbx with needs a command to run: sbx with <sandbox> [--template T] -- <command>")
	}

	if err := ValidateName("sandbox", sandbox); err != nil {
		return err
	}

	// The name lock, taken before the existence check and held until this run has removed what it
	// made. Without it two `sbx with qa-cc` both found the name unused, the second reused the
	// first's containers ("already exists"), and whichever finished first removed the sandbox
	// under the other. Held for the whole run, not just the create: released after the create, an
	// `sbx create X` during the command reused the ephemeral sandbox and printed "ready" for
	// something this run was about to delete. The lock says it is a `with`'s, so create and add
	// refuse it at once. Not waited for here: if anyone holds it, the name is someone else's.
	release, err := slotlock.ClaimEphemeral(sandbox)
	if err != nil {
		return fmt.Errorf("sandbox %q is being created or changed by another sbx right now, and sbx with\n"+
			"     only takes a name nobody is using. Pick another name.\n     %w", sandbox, err)
	}
	defer release()

	// Refused before anything is created, because the teardown removes the sandbox, volumes
	// included. A create over an existing sandbox does not fail - each service reports "already
	// exists" and is reused - so without this, `sbx with X` against a branch sandbox X ran the
	// command and then destroyed X and its data. Confirmed live: a redis key set in X was gone
	// after `sbx with X -- true`.
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

	// What this run made, by container identity. The teardown removes these and nothing else.
	var owned map[string]bool

	err = runScoped(
		func() error {
			err := createWithin(ctx, p, path, sandbox, withOptional, iso, createOpts{healthTimeout: timeout})

			// Under the name lock, and the name was empty when it was taken: everything there
			// now is this run's, including what a failed create left.
			owned = ownedUnits(p, sandbox)

			return err
		},
		func() error { return Ready(ctx, p, sandbox, timeout) },
		func() ([][2]string, error) { return envVars(ctx, p, path, sandbox) },
		func(vars [][2]string) error { return runCommand(ctx, args, vars) },
		func() error {
			bg, cancel := context.WithTimeout(context.Background(), teardownBudget)
			defer cancel()

			if owned == nil {
				return fmt.Errorf("could not list what this run created, so nothing was removed - remove it by hand: sbx rm %s", sandbox)
			}

			return removeOwned(bg, p, sandbox, owned)
		},
		keep,
	)

	// A create whose services never served says how to remove the sandbox, which is right for
	// `sbx create` and wrong here once the teardown has removed it.
	var ns *notServingError
	if !keep && errors.As(err, &ns) {
		if us, lerr := p.List(context.Background(), sandbox); lerr == nil && len(us) == 0 {
			ns.removed = true
		}
	}

	if keep {
		if us, lerr := p.List(context.Background(), sandbox); lerr == nil && len(us) > 0 {
			fmt.Fprintf(os.Stderr, "  kept sandbox %q (--keep). Remove it when you are done: sbx rm %s\n", sandbox, sandbox)
		}
	}

	// Interrupted before the command had an exit status of its own - during create or the wait
	// for ready. Say why it stopped, and exit as the signal says.
	var in *Interrupted
	if errors.As(context.Cause(ctx), &in) {
		var ce *ChildExit
		if !errors.As(err, &ce) {
			if err != nil {
				fmt.Fprintf(os.Stderr, "sbx: %v\n", err)
			}

			return in
		}
	}

	return err
}

func unitKey(u provider.Unit) string { return cmp.Or(u.Instance, u.Ref) }

// ownedUnits lists the sandbox's containers by identity, or nil if they cannot be listed.
func ownedUnits(p provider.Provider, sandbox string) map[string]bool {
	units, err := p.List(context.Background(), sandbox)
	if err != nil {
		return nil
	}

	owned := make(map[string]bool, len(units))
	for _, u := range units {
		owned[unitKey(u)] = true
	}

	return owned
}

// removeOwned is the teardown: remove the containers this run created, and only those.
//
// It used to remove everything under the name, on the grounds that the name was unused when the
// run began. That held only while nobody else used the name meanwhile - and the run's command
// can take an hour. Anything that appeared since (an `sbx add`, a create that waited for this
// one's lock) is someone else's, so it is left, and the reader is told what was left and how to
// remove it. When everything is this run's, the whole sandbox goes, volumes and all, as before.
func removeOwned(ctx context.Context, p provider.Provider, sandbox string, owned map[string]bool) error {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	// A create that failed before its first container existed left nothing, and removing
	// nothing is an error to the provider ("no sandbox") that would bury the real one.
	if len(units) == 0 {
		return nil
	}

	var mine, others []provider.Unit

	for _, u := range units {
		if owned[unitKey(u)] {
			mine = append(mine, u)
		} else {
			others = append(others, u)
		}
	}

	if len(others) == 0 {
		fmt.Fprintf(os.Stderr, "  removing ephemeral sandbox %q\n", sandbox)

		if err := Remove(ctx, p, sandbox); err != nil {
			return fmt.Errorf("%w - remove it by hand: sbx rm %s", err, sandbox)
		}

		return nil
	}

	names := make([]string, 0, len(others))
	for _, u := range others {
		names = append(names, u.Service)
	}

	sort.Strings(names)

	left := strings.Join(names, ", ")

	r, ok := p.(provider.UnitRemover)
	if !ok {
		fmt.Fprintf(os.Stderr, "  left sandbox %q in place: %s appeared under it while the command ran, and this\n"+
			"  backend cannot remove one service at a time. Remove it when nothing uses it: sbx rm %s\n",
			sandbox, left, sandbox)

		return nil
	}

	for _, u := range mine {
		if err := r.RemoveUnit(ctx, u.Ref); err != nil {
			return fmt.Errorf("removing %s: %w - remove it by hand: sbx rm %s", u.Ref, err, sandbox)
		}
	}

	fmt.Fprintf(os.Stderr, "  removed this run's services from %q and left %s, which this run did not create.\n"+
		"  Remove the sandbox when nothing uses it: sbx rm %s\n", sandbox, left, sandbox)

	return nil
}

// runScoped is the lifecycle, with each step injected so it is testable without docker: create,
// then guarantee teardown, then ready, env and run. Teardown fires on every path except when
// keep is set - a failed create, a failed ready, a failed command, a panic.
//
// A failed create is torn down too. It rarely fails before making anything: a health check that
// never passes leaves a running, unhealthy container, and a failed `docker run` can leave one in
// "Created". This used to return here on the grounds that nothing was created, which was false,
// and the next `sbx with` under the same name then refused it as an existing sandbox. What the
// teardown removes is what the create made - see removeOwned.
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
//
// A cancelled context sends the command the signal that cancelled it, not a kill: a test runner
// told SIGTERM gets to write its report and exit, and a Ctrl-C reaches it the way it would
// without sbx in between. It is killed only if it is still running WaitDelay later.
func runCommand(ctx context.Context, args []string, vars [][2]string) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = os.Environ()

	for _, kv := range vars {
		cmd.Env = append(cmd.Env, kv[0]+"="+kv[1])
	}

	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	cmd.Cancel = func() error {
		sig := os.Kill

		var in *Interrupted
		if errors.As(context.Cause(ctx), &in) {
			sig = in.Signal
		}

		err := cmd.Process.Signal(sig)
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			return cmd.Process.Kill() // a signal this platform cannot send, such as SIGINT on Windows
		}

		return err
	}
	cmd.WaitDelay = 10 * time.Second

	if e := cmd.Run(); e != nil {
		var ee *exec.ExitError
		if errors.As(e, &ee) {
			code := ee.ExitCode()

			// -1 is "killed by a signal", which no shell reports: they say 128+signal.
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && code == -1 && ws.Signaled() {
				code = 128 + int(ws.Signal())
			}

			return &ChildExit{Code: code}
		}

		return fmt.Errorf("running %q: %w", args[0], e)
	}

	return nil
}
