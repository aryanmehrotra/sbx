package provider

// A daemon that is slow rather than absent.
//
// Measured at 1m36s to list seven containers on a busy colima, where the same call is
// milliseconds on an idle one. What the dashboard showed was "context deadline exceeded"
// wrapped around an escaped URL, which says what the Go runtime did rather than what happened -
// and the two cases have different fixes: a socket that is not there needs the runtime started,
// one that is not answering needs it looked at.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestASlowDaemonSaysItIsSlowRatherThanQuotingTheRuntime(t *testing.T) {
	// A socket that exists, so this is the daemon being slow rather than absent. Those are
	// different diagnoses with different fixes, and the absent one is checked first.
	sock := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ep := dockerEndpoint{Network: "unix", Address: sock}

	err := listError(ep, context.DeadlineExceeded)
	if err == nil {
		t.Fatal("a deadline produced no error")
	}

	if strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("the message quotes the runtime rather than saying what happened: %v", err)
	}

	for _, want := range []string{"did not answer in time", "busy", "colima status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message never mentions %q: %v", want, err)
		}
	}
}

// An unreachable socket is a different thing and keeps its own message - and keeps wrapping the
// cause, so `errors.Is` still works on it.
func TestAnUnreachableDaemonKeepsItsCause(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ep := dockerEndpoint{Network: "unix", Address: sock}
	cause := errors.New("connection reset by peer")

	err := listError(ep, cause)

	if !errors.Is(err, cause) {
		t.Errorf("the cause was dropped: %v", err)
	}

	if !strings.Contains(err.Error(), "listing sandboxes via") {
		t.Errorf("an unreachable daemon lost its message: %v", err)
	}
}

// A socket that is not there is a runtime that is not running, and that is worth saying in
// those words. The dial error underneath - "connect: no such file or directory" against a path
// nobody typed - describes the symptom and names neither the thing that is down nor the command
// that brings it back.
func TestARuntimeThatIsNotRunningIsNamed(t *testing.T) {
	ep := dockerEndpoint{Network: "unix", Address: "/Users/x/.colima/default/docker.sock"}

	err := listError(ep, errors.New("connect: no such file or directory"))
	if err == nil {
		t.Fatal("a missing socket produced no error")
	}

	for _, want := range []string{"colima is not running", "colima start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message never says %q: %v", want, err)
		}
	}

	// And it says the sandboxes are still there, because the obvious fear on seeing this is
	// that they are not.
	if !strings.Contains(err.Error(), "survive") {
		t.Errorf("it does not say the sandboxes survive: %v", err)
	}
}

// Each runtime puts its socket somewhere characteristic, which is the only evidence left once
// the thing is down: a stopped runtime cannot be asked what it is.
func TestTheRuntimeIsNamedFromWhereItsSocketIs(t *testing.T) {
	for path, want := range map[string]string{
		"/Users/x/.colima/default/docker.sock": "colima",
		"/Users/x/.rd/docker.sock":             "Rancher Desktop",
		"/Users/x/.local/share/podman.sock":    "podman",
		"/Users/x/.docker/run/docker.sock":     "Docker Desktop",
		"/var/run/docker.sock":                 "the container runtime",
	} {
		name, start := runtimeHint(dockerEndpoint{Network: "unix", Address: path})

		if !strings.Contains(name, want) {
			t.Errorf("%s was called %q, want something with %q", path, name, want)
		}

		if start == "" {
			t.Errorf("%s has no command to start it", path)
		}
	}
}
