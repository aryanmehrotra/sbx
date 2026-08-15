package main

import (
	"strings"
	"sync"
	"testing"
)

// recorder is an observer that remembers, so the tests assert on what Ingest decoded rather
// than on what a metrics backend happened to do with it.
type recorder struct {
	mu      sync.Mutex
	wakes   []int64
	sleeps  int
	failed  int
	sources []string
}

func (r *recorder) Wake(sandbox, service string, ms int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.wakes = append(r.wakes, ms)
	r.sources = append(r.sources, key(sandbox, service))
}

func (r *recorder) Sleep(string, string)      { r.mu.Lock(); r.sleeps++; r.mu.Unlock() }
func (r *recorder) WakeFailed(string, string) { r.mu.Lock(); r.failed++; r.mu.Unlock() }

func TestIngest(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		wakes  []int64
		sleeps int
		failed int
	}{
		{
			name:  "a wake carries its duration",
			in:    `{"level":"INFO","sandbox":"br","service":"redis","event":"woke","durationMs":278}`,
			wakes: []int64{278},
		},
		{
			name:   "a sleep is counted, and is not a wake",
			in:     `{"sandbox":"br","service":"redis","event":"slept","durationMs":4000}`,
			sleeps: 1,
		},
		{
			name:   "a failed wake is neither a wake nor a sleep",
			in:     `{"sandbox":"br","service":"redis","event":"wakeFailed"}`,
			failed: 1,
		},
		{
			// The daemon logs plenty that is not an event. Counting those would inflate
			// every number on the dashboard.
			name: "untagged lines are ignored",
			in: `{"level":"INFO","sandbox":"br","service":"redis","message":"listening on :20000"}
{"level":"INFO","message":"sbx dev · provider docker"}`,
		},
		{
			// A console that dies on a truncated line takes the monitoring down with it,
			// which is precisely when someone needs it.
			name: "malformed lines are dropped, the rest still counts",
			in: `{"sandbox":"br","service":"redis","event":"woke","durationMs":100}
{"sandbox":"br","service":"red
{"sandbox":"br","service":"redis","event":"woke","durationMs":200}`,
			wakes: []int64{100, 200},
		},
		{
			name: "terminal-formatted output is not JSON and is skipped",
			in: `INFO [14:16:40] br/redis  woke in 278ms
{"sandbox":"br","service":"redis","event":"woke","durationMs":278}`,
			wakes: []int64{278},
		},
		{
			// An event a newer daemon emits and this build has never heard of must not be
			// guessed at, and must not stop the stream.
			name: "unknown events are passed over",
			in: `{"sandbox":"br","service":"redis","event":"hibernated","durationMs":5}
{"sandbox":"br","service":"redis","event":"woke","durationMs":9}`,
			wakes: []int64{9},
		},
		{
			name: "an empty stream is not an error",
			in:   "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r recorder
			if err := Ingest(strings.NewReader(c.in), &r); err != nil {
				t.Fatalf("Ingest: %v", err)
			}

			if len(r.wakes) != len(c.wakes) {
				t.Fatalf("wakes: got %v, want %v", r.wakes, c.wakes)
			}

			for i, want := range c.wakes {
				if r.wakes[i] != want {
					t.Errorf("wake %d: got %dms, want %dms", i, r.wakes[i], want)
				}
			}

			if r.sleeps != c.sleeps {
				t.Errorf("sleeps: got %d, want %d", r.sleeps, c.sleeps)
			}

			if r.failed != c.failed {
				t.Errorf("wake failures: got %d, want %d", r.failed, c.failed)
			}
		})
	}
}

// Two sandboxes may each run a postgres. Collapsing them would report one sandbox's wakes
// against another's name.
func TestIngestKeepsSandboxesApart(t *testing.T) {
	var r recorder

	in := `{"sandbox":"a","service":"postgres","event":"woke","durationMs":1}
{"sandbox":"b","service":"postgres","event":"woke","durationMs":2}`

	if err := Ingest(strings.NewReader(in), &r); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if got := r.sources; len(got) != 2 || got[0] != "a/postgres" || got[1] != "b/postgres" {
		t.Fatalf("sources: got %v, want [a/postgres b/postgres]", got)
	}
}

func TestStoreCountsAndState(t *testing.T) {
	s := &store{svc: map[string]*service{}}

	s.Wake("br", "redis", 278)
	s.Wake("br", "redis", 190)
	s.Sleep("br", "redis")
	s.WakeFailed("br", "redis")

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot: got %d services, want 1", len(snap))
	}

	got := snap[0]
	if got.Wakes != 2 || got.Sleeps != 1 || got.Failures != 1 {
		t.Errorf("counts: wakes=%d sleeps=%d failures=%d, want 2/1/1", got.Wakes, got.Sleeps, got.Failures)
	}

	if got.LastMs != 190 {
		t.Errorf("last wake: got %dms, want 190ms", got.LastMs)
	}

	// A sleep after a wake means asleep. State is the last thing that happened, not a tally.
	if got.Awake {
		t.Error("awake after a sleep event")
	}
}

// A failed wake must not report the service as awake: it is exactly as asleep as before.
func TestFailedWakeDoesNotMarkAwake(t *testing.T) {
	s := &store{svc: map[string]*service{}}
	s.WakeFailed("br", "redis")

	if s.Snapshot()[0].Awake {
		t.Error("a failed wake marked the service awake")
	}
}
