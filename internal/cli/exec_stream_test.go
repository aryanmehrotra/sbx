package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// execFake is a provider whose one service echoes stdin and exits 7, the way `sh -c 'cat;
// exit 7'` does in a real container. Exec answers the way the capturing path really did - no
// stdin reached the workload, and the status arrived as an *ExitError - so a CLI that still
// goes through it fails both halves of the test below.
type execFake struct {
	provider.Provider
	units   []provider.Unit
	started []string
}

func (f *execFake) Name() string { return "fake" }

func (f *execFake) List(context.Context, string) ([]provider.Unit, error) { return f.units, nil }

func (f *execFake) Start(_ context.Context, ref string) error {
	f.started = append(f.started, ref)
	return nil
}

func (f *execFake) Probe(context.Context, string) (bool, bool) { return true, true }

func (f *execFake) Exec(context.Context, string, []string) (string, error) {
	return "", &provider.ExitError{Code: 7}
}

func (f *execFake) ExecStream(_ context.Context, _ string, _ []string, in io.Reader, out, _ io.Writer) (int, error) {
	if in != nil {
		if _, err := io.Copy(out, in); err != nil {
			return -1, err
		}
	}

	return 7, nil
}

// swapStdio points os.Stdin at a pipe holding input and os.Stdout at a pipe the test reads,
// because `sbx exec` is wired to the process's own stdio and that is exactly what is under test.
func swapStdio(t *testing.T, input string) (read func() string) {
	t.Helper()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		_, _ = io.WriteString(inW, input)
		_ = inW.Close()
	}()

	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW

	t.Cleanup(func() { os.Stdin, os.Stdout = oldIn, oldOut })

	return func() string {
		os.Stdout = oldOut
		_ = outW.Close()

		b, _ := io.ReadAll(outR)

		return string(b)
	}
}

// `echo hi | sbx exec sb svc sh -c 'cat; exit 7'` must print hi and exit 7. The capturing path
// dropped stdin (nothing printed) and replaced every non-zero status with 1, which made `sbx
// exec` useless both for piping a SQL file into psql and for a CI step gating on the result.
func TestExecStreamsStdinAndPassesTheExitStatusThrough(t *testing.T) {
	t.Setenv("SBX_HISTORY", t.TempDir()+"/h.jsonl")

	p := &execFake{units: []provider.Unit{{Sandbox: "sb", Service: "svc", Ref: "sbx-sb-svc", Running: true}}}
	read := swapStdio(t, "hi\n")

	err := Exec(context.Background(), p, "sb", "svc", []string{"sh", "-c", "cat; exit 7"}, false)

	if got := read(); got != "hi\n" {
		t.Errorf("stdout = %q, want the piped stdin %q echoed back", got, "hi\n")
	}

	var cs interface{ ChildStatus() int }
	if !errors.As(err, &cs) || cs.ChildStatus() != 7 {
		t.Fatalf("Exec returned %v (%T); want an error whose ChildStatus() is 7 so `sbx exec` exits 7", err, err)
	}
}

// A zero exit is success, not an error carrying status 0.
func TestExecZeroExitIsNil(t *testing.T) {
	t.Setenv("SBX_HISTORY", t.TempDir()+"/h.jsonl")

	p := &zeroExec{execFake{units: []provider.Unit{{Sandbox: "sb", Service: "svc", Ref: "r", Running: true}}}}
	read := swapStdio(t, "")

	err := Exec(context.Background(), p, "sb", "svc", []string{"true"}, false)
	_ = read()

	if err != nil {
		t.Fatalf("Exec of a command that exited 0 returned %v", err)
	}
}

type zeroExec struct{ execFake }

func (z *zeroExec) ExecStream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
	return 0, nil
}

func (z *zeroExec) Exec(context.Context, string, []string) (string, error) { return "", nil }

// `sbx exec` against a sleeping service wakes it, and that wake is something that happened to
// the sandbox: `sbx history` promises every wake and sleep, and a CLI wake is as real as one the
// daemon made on a connection. Recorded as the CLI's, not the daemon's, or the journal
// misattributes a person's action to the idle policy.
func TestExecWakeIsJournalledAsTheCLIs(t *testing.T) {
	t.Setenv("SBX_HISTORY", t.TempDir()+"/h.jsonl")

	p := &execFake{units: []provider.Unit{{Sandbox: "sb", Service: "svc", Ref: "sbx-sb-svc", Running: false}}}
	read := swapStdio(t, "")

	_ = Exec(context.Background(), p, "sb", "svc", []string{"true"}, false)
	_ = read()

	if len(p.started) != 1 {
		t.Fatalf("the sleeping service was not started: %v", p.started)
	}

	assertEvent(t, "sb", "svc", "woke")
}

// `sbx sleep` stops services by hand. Each stop is a sleep in the journal, attributed to the CLI.
func TestSleepIsJournalledAsTheCLIs(t *testing.T) {
	t.Setenv("SBX_HISTORY", t.TempDir()+"/h.jsonl")

	p := &sleeper{units: []provider.Unit{
		{Sandbox: "agent-42", Service: "postgres", Ref: "sbx-agent-42-postgres", Running: true},
		{Sandbox: "agent-42", Service: "browser", Ref: "sbx-agent-42-browser", Running: false},
	}}

	if err := Sleep(context.Background(), p, "agent-42"); err != nil {
		t.Fatal(err)
	}

	assertEvent(t, "agent-42", "postgres", "slept")

	recs, _ := history.Read(history.Filter{Kind: "event"})
	for _, r := range recs {
		if r.Service == "browser" {
			t.Errorf("the already-asleep service was journalled as slept: %+v", r)
		}
	}
}

func assertEvent(t *testing.T, sandbox, service, event string) {
	t.Helper()

	recs, err := history.Read(history.Filter{Sandbox: sandbox, Kind: "event"})
	if err != nil {
		t.Fatal(err)
	}

	var seen []string

	for _, r := range recs {
		seen = append(seen, r.Service+":"+r.Event+"@"+r.Actor)

		if r.Service == service && r.Event == event {
			if r.Actor != "cli" {
				t.Errorf("%s/%s %s recorded with actor %q, want \"cli\"", sandbox, service, event, r.Actor)
			}

			return
		}
	}

	t.Fatalf("no %q event for %s/%s in the journal; it has: [%s]", event, sandbox, service, strings.Join(seen, ", "))
}
