package main

import (
	"context"

	"gofr.dev/pkg/gofr/metrics"
)

// sink is the seam between the log stream and the metrics backend. It updates the in-memory
// store that /api/sandboxes serves and the Prometheus series that /metrics serves, and it is
// the only thing in this program that knows GoFr exists — which is why ingest.go and its
// tests do not import it.
type sink struct{ m metrics.Manager }

// Labels are sandbox and service. A single series across all sandboxes would answer "are
// wakes slow" and never "which branch is slow", and the second question is the one anyone
// actually has.
func (s *sink) Wake(sandbox, service string, ms int64) {
	state.Wake(sandbox, service, ms)

	ctx := context.Background()
	s.m.IncrementCounter(ctx, "sbx_wakes_total", "sandbox", sandbox, "service", service)
	s.m.RecordHistogram(ctx, "sbx_wake_duration_ms", float64(ms), "sandbox", sandbox, "service", service)
}

func (s *sink) Sleep(sandbox, service string) {
	state.Sleep(sandbox, service)
	s.m.IncrementCounter(context.Background(), "sbx_sleeps_total", "sandbox", sandbox, "service", service)
}

func (s *sink) WakeFailed(sandbox, service string) {
	state.WakeFailed(sandbox, service)
	s.m.IncrementCounter(context.Background(), "sbx_wake_failures_total", "sandbox", sandbox, "service", service)
}
