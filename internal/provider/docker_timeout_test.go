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
	"strings"
	"testing"
)

func TestASlowDaemonSaysItIsSlowRatherThanQuotingTheRuntime(t *testing.T) {
	ep := dockerEndpoint{Network: "unix", Address: "/Users/x/.colima/default/docker.sock"}

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
	ep := dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"}
	cause := errors.New("no such file or directory")

	err := listError(ep, cause)

	if !errors.Is(err, cause) {
		t.Errorf("the cause was dropped: %v", err)
	}

	if !strings.Contains(err.Error(), "listing sandboxes via") {
		t.Errorf("an unreachable daemon lost its message: %v", err)
	}
}
