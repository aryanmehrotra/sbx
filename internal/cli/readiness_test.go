package cli

import (
	"net"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// What `sbx create` says at the end, which used to be a claim rather than a check.
//
// It printed "ready. connecting wakes it" unconditionally. With no daemon running that is
// false in the most common first-run state: the sandbox exists, `sbx env` prints an address,
// and the first connection is refused with nothing anywhere explaining why.

// A port that is actually served must read as ready, whatever the presence file says.
//
// The presence file can be wrong in exactly this direction: sbx supports several daemons on
// one machine (one per worktree, per CI job) and they share one file, so whichever exits
// last removes it while the others are still serving. Dialling first makes that harmless.
func TestReadinessTrustsTheOpenPortOverThePresenceFile(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			_ = c.Close()
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port

	got := readiness([]provider.Endpoint{{Host: "127.0.0.1", Port: port}})
	if !strings.HasPrefix(got, "ready.") {
		t.Errorf("a served port did not read as ready:\n%s", got)
	}
}

// Nothing listening, and (in a test process) no daemon presence: say so, and say what to run.
func TestReadinessSaysWhatToRunWhenNothingAnswers(t *testing.T) {
	// An empty HOME, so daemon.Running() cannot find this machine's real presence file.
	// Without it this test takes 30 seconds on any machine that happens to have a daemon
	// running - it would wait for the sandbox to be picked up, which never happens because
	// there is no sandbox.
	t.Setenv("HOME", t.TempDir())

	// A port bound and immediately released - nothing is listening on it now.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	got := readiness([]provider.Endpoint{{Host: "127.0.0.1", Port: port}})

	if strings.HasPrefix(got, "ready.") {
		t.Fatalf("claimed ready with nothing listening:\n%s", got)
	}

	// It has to name the command, or the message is just bad news.
	if !strings.Contains(got, "sbx serve") {
		t.Errorf("the message does not say what to run:\n%s", got)
	}
}

// A remote docker host's ports are not this machine's to judge, and blocking on them would
// make every create against a remote host wait for a timeout.
func TestReadinessDoesNotJudgeARemoteHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	got := readiness([]provider.Endpoint{{Host: "10.0.0.7", Port: 20000}})
	if !strings.HasPrefix(got, "ready.") {
		t.Errorf("judged a remote host's ports:\n%s", got)
	}
}
