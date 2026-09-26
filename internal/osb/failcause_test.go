package osb

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A Failed sandbox is only useful if it says why. Seen on a CPU-starved Linux box: ten parallel
// creates all ended `Failed (runtime_error)` whose message named no cause - the container was
// killed by a signal, printed nothing, and "Its last output:" was followed by nothing at all.

// logSink collects the daemon log lines for one sandbox. Observers cannot be removed, so each
// test filters to the ids it made.
type logSink struct {
	mu    sync.Mutex
	lines []logs.Entry
}

var sink = func() *logSink {
	s := &logSink{}
	logs.Default.Observe(func(e logs.Entry) {
		s.mu.Lock()
		s.lines = append(s.lines, e)
		s.mu.Unlock()
	})

	return s
}()

func (s *logSink) of(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string

	for _, e := range s.lines {
		if e.Sandbox == id {
			out = append(out, e.Level+" "+e.Message)
		}
	}

	return out
}

// assertCarried checks the cause reached the status, the daemon log and the history.
func assertCarried(t *testing.T, h *harness, got sandboxJSON, reason string, want ...string) {
	t.Helper()

	if got.Status.State != stateFailed || got.Status.Reason != reason {
		t.Fatalf("status %+v, want Failed %s", got.Status, reason)
	}

	if strings.TrimSpace(got.Status.Message) == "" {
		t.Fatalf("Failed %s with an empty message: nobody can act on that", reason)
	}

	for _, w := range want {
		if !strings.Contains(got.Status.Message, w) {
			t.Errorf("status.message %q does not carry %q", got.Status.Message, w)
		}
	}

	var logged string

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logged = strings.Join(sink.of(got.ID), "\n")
		if strings.Contains(logged, reason) {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	if !strings.Contains(logged, "ERROR osb: Failed ("+reason+")") || !strings.Contains(logged, want[0]) {
		t.Errorf("daemon log for %s = %q, want an error line naming %s and %q", got.ID, logged, reason, want[0])
	}

	h.srv.wg.Wait() // history is appended off the request path

	recs, err := history.Read(history.Filter{Sandbox: got.ID})
	if err != nil {
		t.Fatal(err)
	}

	found := false

	for _, r := range recs {
		if r.Event == "failed" && strings.HasPrefix(r.Message, reason+": ") && strings.Contains(r.Message, want[0]) {
			found = true
		}

		if strings.Contains(r.Message, "sbx-token-") || strings.Contains(r.Message, tokenEnv) {
			t.Errorf("history carries a secret: %q", r.Message)
		}
	}

	if !found {
		t.Errorf("history for %s has no failed event carrying %q: %+v", got.ID, want[0], recs)
	}
}

func withExitReporter(h *harness, o *Options) { o.Provider = fakeExitDocker{h.p} }

// The case from the field: killed by a signal, nothing printed. The message must say how it
// stopped - exit code, OOM - and that it printed nothing, instead of ending on "Its last output:".
func TestRuntimeErrorCarriesTheExitStateWhenTheContainerPrintedNothing(t *testing.T) {
	h := newHarness(t, withExitReporter)
	h.p.exitOnStart = true
	h.p.logs = ""
	h.p.exit = &provider.ExitState{Status: "exited", ExitCode: 137, OOMKilled: true}

	got := h.create(minimalCreate())

	assertCarried(t, h, got, "runtime_error", "exit code 137", "OOMKilled", "It printed nothing")
}

// A container docker created and could not start is not one that "exited": the engine's own
// error is the whole cause, and the logs are empty.
func TestRuntimeErrorCarriesTheEnginesStartError(t *testing.T) {
	h := newHarness(t, withExitReporter)
	h.p.exitOnStart = true
	h.p.logs = ""
	h.p.exit = &provider.ExitState{Status: "created", ExitCode: 128,
		Error: "OCI runtime create failed: cpu.cfs_quota_us: permission denied"}

	got := h.create(minimalCreate())

	assertCarried(t, h, got, "runtime_error", "cfs_quota_us", "never started")
}

// A provider that cannot report exit states still produces a message that says so, rather
// than a blank where the cause should be.
func TestRuntimeErrorSaysWhenTheProviderCannotReportExitState(t *testing.T) {
	h := newHarness(t)
	h.p.exitOnStart = true
	h.p.logs = ""

	got := h.create(minimalCreate())

	assertCarried(t, h, got, "runtime_error", "does not report exit states", "It printed nothing")
}

func TestCreateFailedCarriesTheEngineError(t *testing.T) {
	h := newHarness(t)
	h.p.createErr = errors.New("docker run: exit status 125: port is already allocated")

	got := h.create(minimalCreate())

	assertCarried(t, h, got, "create_failed", "port is already allocated")
}

// An error with no text used to become a Failed with no message. It now says so.
func TestAFailureWithAnEmptyCauseStillSaysSomething(t *testing.T) {
	h := newHarness(t)
	h.p.createErr = errors.New("")

	got := h.create(minimalCreate())

	assertCarried(t, h, got, "create_failed", "no cause was reported for create_failed")
}

func TestProvisionTimeoutCarriesTheLastPingError(t *testing.T) {
	h := newHarness(t, func(_ *harness, o *Options) { o.ReadyTimeout = 100 * time.Millisecond })

	h.mu.Lock()
	h.pingErr = errors.New("connection refused")
	h.mu.Unlock()

	var created sandboxJSON
	h.do("POST", "/v1/sandboxes", minimalCreate(), &created)

	// The deadline is on the injected clock, so move it.
	time.Sleep(50 * time.Millisecond)
	h.advance(time.Second)

	got := h.waitState(created.ID, stateFailed)

	assertCarried(t, h, got, "provision_timeout", "connection refused", "hello from the container")
}
