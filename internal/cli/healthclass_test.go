package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// execStub is a Provider whose Exec fails the way a given runtime fails.
type execStub struct {
	provider.Provider

	out string
	err error
}

func (e *execStub) Exec(context.Context, string, []string) (string, error) { return e.out, e.err }

// A health command is only "impossible" when the SHELL says so, never when the runtime is
// still starting the container.
//
// sbx probes as soon as the workload exists, so on kubernetes the pod is usually still
// scheduling and the apiserver answers `container not found ("app")`. That contains the words
// "not found", which the classifier matched - so `sbx create --provider kubernetes` failed
// every template with a health check and reported "the health command cannot run in this
// image", telling the reader to go and check whether the tool was installed.
//
// Measured against nginx:alpine in a real minikube: wget is at /usr/bin/wget and the exact
// command exits 0 a moment later. A wrong diagnosis that sends somebody to fix the wrong thing
// is worse than no diagnosis, and this one rejected a spec that was correct.
func TestOnlyTheShellDecidesAHealthCommandCanNeverPass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		out   string
		err   string
		fatal bool
	}{
		// The shell's own verdicts. Waiting cannot fix these.
		{"command missing", "sh: wget: not found", "exit status 127", true},
		{"not executable", "sh: /x: Permission denied", "exit status 126", true},
		{"missing, status only in the error", "no output", "exit status 127", true},

		// The runtime, mid-start. Waiting is exactly what fixes these.
		{
			name: "kubernetes, pod not started",
			out:  "no output",
			err:  `error: Internal error occurred: unable to upgrade connection: container not found ("app")`,
		},
		{"kubernetes, still creating", "no output", "container is in ContainerCreating", false},
		{"kubernetes, initialising", "no output", "PodInitializing", false},
		{"docker, not started yet", "no output", "Error response from daemon: container is not running", false},
		{"docker, between create and start", "no output", "Error: No such container: sbx-x-y", false},

		// An ordinary failing check: the service is simply not up yet.
		{"service not listening", "wget: can't connect to remote host", "exit status 1", false},
	} {
		p := &execStub{out: tc.out, err: errors.New(tc.err)}

		why, fatal := healthWillNeverPass(context.Background(), p, "ref", "wget -qO- http://127.0.0.1/")

		if fatal != tc.fatal {
			t.Errorf("%s: fatal=%v want %v (why=%q)\n  out=%q err=%q",
				tc.name, fatal, tc.fatal, why, tc.out, tc.err)
		}

		if fatal && strings.TrimSpace(why) == "" {
			t.Errorf("%s: called it fatal but gave no reason", tc.name)
		}
	}
}

// A command that simply works is never fatal.
func TestAHealthCommandThatSucceedsIsNotFatal(t *testing.T) {
	p := &execStub{out: "", err: nil}

	if _, fatal := healthWillNeverPass(context.Background(), p, "ref", "true"); fatal {
		t.Error("a health command that exited 0 was called impossible")
	}
}
