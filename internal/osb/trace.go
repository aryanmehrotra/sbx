package osb

// Per-phase timing of one create, for finding where the time goes.
//
// Off unless SBX_OSB_TRACE is set, because it is a line per phase per sandbox and a burst of a
// hundred creates would otherwise bury everything else the daemon logs. When on, each phase is
// logged as it happens with the milliseconds since the POST was accepted, so a create that is
// slow shows the one phase that was, and the table in docs/BENCHMARKS.md is read off these
// lines rather than guessed.

import (
	"os"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
)

// tracer stamps phases against the moment each sandbox's create was accepted.
type tracer struct {
	on bool

	mu    sync.Mutex
	start map[string]time.Time
}

func newTracer() *tracer {
	return &tracer{on: os.Getenv("SBX_OSB_TRACE") != "", start: map[string]time.Time{}}
}

// begin records the accept time. A no-op when tracing is off, so the map never grows.
func (t *tracer) begin(id string) {
	if !t.on {
		return
	}

	t.mu.Lock()
	t.start[id] = time.Now()
	t.mu.Unlock()

	logs.Default.Info(id, service, "trace +0ms accepted")
}

// mark logs a phase. Phases after the sandbox is Running (the SDK's GETs, the endpoint lookup)
// are marked too: they are part of what a client waits for, and the server is the one place
// that sees all of them.
func (t *tracer) mark(id, phase string) {
	if !t.on {
		return
	}

	t.mu.Lock()
	st, ok := t.start[id]
	t.mu.Unlock()

	if !ok {
		return
	}

	logs.Default.Info(id, service, "trace +%dms %s", time.Since(st).Milliseconds(), phase)
}

// end forgets a sandbox, so a long-running traced daemon does not keep every id it ever made.
func (t *tracer) end(id string) {
	if !t.on {
		return
	}

	t.mu.Lock()
	delete(t.start, id)
	t.mu.Unlock()
}
