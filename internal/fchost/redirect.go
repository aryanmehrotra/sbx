package fchost

// `sbx create/list/env/exec/logs/rm --provider firecracker` on a host that cannot run
// Firecracker itself.
//
// The command is run, unchanged, by the linux sbx inside the helper VM. Not reimplemented over
// an API: the in-VM sbx IS the firecracker provider, so every command it has works the day it
// lands there, with the same flags and the same errors. Output that names ports (env, list) is
// right on this machine too, because the host side mirrors each port at the same number.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Firecracker is the provider kind this package fronts.
const Firecracker = "firecracker"

// redirected is every command that takes --provider and acts on sandboxes. Left out on purpose:
// serve (Front handles it), ui/url/ssh (they open things on THIS machine: a terminal UI, a
// public tunnel, an ssh server), and with (it runs a host command, which inside the VM would be
// a different command in a different filesystem).
var redirected = map[string]bool{
	"create": true, "env": true, "ready": true, "wake": true, "sleep": true, "add": true,
	"list": true, "rm": true, "exec": true, "logs": true, "cp": true, "snapshot": true,
	"fork": true, "checkpoint": true, "resume": true, "gc": true, "prewarm": true,
	"selftest": true, "egress": true,
}

// ProviderKind is the provider a command line asks for: --provider, else SBX_PROVIDER_KIND.
// It stops at "--", after which arguments belong to the command being run in the sandbox.
func ProviderKind(args []string, getenv func(string) string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}

		for _, p := range []string{"--provider", "-provider"} {
			if a == p && i+1 < len(args) {
				return args[i+1]
			}

			if v, ok := strings.CutPrefix(a, p+"="); ok {
				return v
			}
		}
	}

	return getenv("SBX_PROVIDER_KIND")
}

// Wants reports whether a command line is a firecracker one this package must handle.
func Wants(cmd string, args []string, getenv func(string) string) bool {
	return (redirected[cmd] || cmd == "serve") && ProviderKind(args, getenv) == Firecracker
}

// GuestArgv is the command run inside the VM. The provider goes in the environment rather than
// as a flag, because appending --provider to `exec`'s argv would hand it to the command being
// executed, and prepending it would land before the sandbox name some commands read first.
func GuestArgv(cmd string, args []string) []string {
	argv := []string{"env", "SBX_PROVIDER_KIND=" + Firecracker}

	if busy := hostBusySlots(); busy != "" {
		argv = append(argv, provider.HostBusySlotsEnv+"="+busy)
	}

	return append(append(argv, guestBinary, cmd), args...)
}

// Refusal formats a Refused backend the way sbx errors read.
func Refusal(b Backend) error {
	return fmt.Errorf("--provider firecracker: %s\n     %s", b.Reason, b.Next)
}

// Redirect runs a firecracker command line wherever it can run. handled=false means this host
// runs Firecracker directly and the caller should carry on as normal.
func Redirect(ctx context.Context, version, cmd string, args []string) (handled bool, code int) {
	b := Detect(Host())

	switch b.Kind {
	case Direct:
		return false, 0
	case Refused:
		fmt.Fprintf(os.Stderr, "sbx: %v\n", Refusal(b))

		return true, 1
	}

	// Progress goes to stderr: `eval "$(sbx env x --provider firecracker)"` reads stdout, and a
	// first-use "creating helper VM" line there would be evaluated as shell.
	m, err := NewManager(b.Helper, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx: %v\n", err)

		return true, 1
	}

	return true, m.redirect(ctx, version, cmd, args, os.Stdin, os.Stdout, os.Stderr)
}

func (m *Manager) redirect(ctx context.Context, version, cmd string, args []string,
	stdin io.Reader, stdout, stderr io.Writer,
) int {
	st, err := m.Status(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "sbx: %v\n", err)

		return 1
	}

	// Started on demand. Only when not running: a running VM was ensured by whoever started
	// it, and re-hashing the binary on every `sbx list` would put a round trip on each one.
	if st != Running {
		if err := m.Ensure(ctx, EnsureOptions{Version: version}); err != nil {
			fmt.Fprintf(stderr, "sbx: %v\n", err)

			return 1
		}
	}

	dir, _ := os.Getwd()

	sh, err := m.Shell(ctx, dir, isTerminal(stdin) && isTerminal(stdout), GuestArgv(cmd, args))
	if err != nil {
		fmt.Fprintf(stderr, "sbx: reaching the helper VM: %v\n", err)

		return 1
	}

	err = m.Run.Run(ctx, Cmd{Argv: sh, Stdin: stdin, Stdout: stdout, Stderr: stderr})

	var exit *exec.ExitError

	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	default:
		fmt.Fprintf(stderr, "sbx: %v\n", err)

		return 1
	}
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}

	fi, err := f.Stat()

	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
