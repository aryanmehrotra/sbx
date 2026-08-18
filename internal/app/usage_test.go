package app

// Which stream usage goes to is part of the interface.
//
// `sbx --help` was asked for, so it belongs on stdout where it can be piped into a pager or
// grepped. Usage printed because the command was wrong is a diagnostic and belongs on stderr,
// where it cannot be mistaken for output by something reading the pipe.
//
// It shipped the other way round in v0.1.0 - everything on stderr - and was caught by the
// Homebrew formula's test block, which read stdout, got an empty string, and failed. A user
// piping `sbx --help` into `grep` would have hit exactly the same nothing.

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// capture runs fn with os.Stdout and os.Stderr replaced, and returns what each received.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	done := make(chan [2]string, 1)

	go func() {
		var o, e bytes.Buffer
		_, _ = o.ReadFrom(outR)
		_, _ = e.ReadFrom(errR)
		done <- [2]string{o.String(), e.String()}
	}()

	fn()

	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()

	got := <-done

	return got[0], got[1]
}

func TestHelpGoesToStdoutAndErrorsGoToStderr(t *testing.T) {
	t.Run("an explicit --help is output", func(t *testing.T) {
		for _, flag := range []string{"--help", "-h", "help"} {
			stdout, stderr := capture(t, func() {
				if err := dispatch(flag, nil); err != nil {
					t.Errorf("%s returned an error: %v", flag, err)
				}
			})

			if !strings.Contains(stdout, "sandboxes") {
				t.Errorf("%s put nothing on stdout, so `sbx %s | grep` finds nothing", flag, flag)
			}

			if stderr != "" {
				t.Errorf("%s also wrote %d bytes to stderr", flag, len(stderr))
			}
		}
	})

	t.Run("usage after a bad command is a diagnostic", func(t *testing.T) {
		stdout, stderr := capture(t, func() {
			if err := dispatch("nosuchcommand", nil); err == nil {
				t.Error("an unknown command returned no error")
			}
		})

		if !strings.Contains(stderr, "sandboxes") {
			t.Error("usage for an unknown command did not go to stderr")
		}

		if stdout != "" {
			t.Errorf("an unknown command wrote %d bytes to stdout, which a pipe would read "+
				"as output", len(stdout))
		}
	})
}
