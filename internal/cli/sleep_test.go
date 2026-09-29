package cli

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// sleeper records which refs were stopped, so a test can assert Sleep parks exactly the running
// services and leaves the asleep ones alone. Stop is called concurrently, so it locks.
type sleeper struct {
	provider.Provider
	units []provider.Unit
	delay time.Duration // how long each Stop takes, standing in for docker's grace period

	mu       sync.Mutex
	stopped  []string
	unpaused []string
	began    map[string]time.Time
	ended    map[string]time.Time
}

func (s *sleeper) List(context.Context, string) ([]provider.Unit, error) { return s.units, nil }
func (s *sleeper) Stop(_ context.Context, ref string) error {
	s.mu.Lock()
	if s.began == nil {
		s.began, s.ended = map[string]time.Time{}, map[string]time.Time{}
	}
	s.began[ref] = time.Now()
	s.mu.Unlock()

	time.Sleep(s.delay)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.stopped = append(s.stopped, ref)
	s.ended[ref] = time.Now()

	return nil
}

// pausingSleeper is a sleeper on a backend that can freeze, so it can be asked to thaw.
type pausingSleeper struct{ *sleeper }

func (s pausingSleeper) Pause(context.Context, string) error { return nil }
func (s pausingSleeper) Unpause(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.unpaused = append(s.unpaused, ref)

	return nil
}

func (s *sleeper) stoppedSorted() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	got := slices.Clone(s.stopped)
	slices.Sort(got)

	return strings.Join(got, ",")
}

// Sleep stops every RUNNING service and skips the ones already asleep - the pair to Ready, and
// an explicit override of the idle timer rather than a second lifecycle owner.
func TestSleepStopsRunningServicesOnly(t *testing.T) {
	p := &sleeper{units: []provider.Unit{
		{Sandbox: "agent-42", Service: "postgres", Ref: "sbx-agent-42-postgres", Running: true},
		{Sandbox: "agent-42", Service: "redis", Ref: "sbx-agent-42-redis", Running: true},
		{Sandbox: "agent-42", Service: "browser", Ref: "sbx-agent-42-browser", Running: false},
	}}

	if err := Sleep(context.Background(), p, "agent-42"); err != nil {
		t.Fatal(err)
	}

	got := p.stoppedSorted()
	want := "sbx-agent-42-postgres,sbx-agent-42-redis"
	if got != want {
		t.Errorf("Sleep stopped %q, want the two running services %q (the asleep one must be left alone)", got, want)
	}
}

// A sandbox that is already fully asleep stops nothing and does not error.
func TestSleepOnAnAlreadyAsleepSandboxIsANoOp(t *testing.T) {
	p := &sleeper{units: []provider.Unit{
		{Sandbox: "x", Service: "db", Ref: "sbx-x-db", Running: false},
	}}

	if err := Sleep(context.Background(), p, "x"); err != nil {
		t.Fatal(err)
	}

	if len(p.stopped) != 0 {
		t.Errorf("Sleep stopped %v on an already-asleep sandbox", p.stopped)
	}
}

// A frozen service (on_idle: freeze, or a paused container) is not running, but it is holding
// every byte of its memory. `sbx sleep` used to skip it and say "already asleep" - the one state
// where sleeping is the whole point. It is thawed first: a microVM's sleep asks the frozen guest
// to seal, which it cannot answer.
func TestSleepStopsAFrozenService(t *testing.T) {
	s := &sleeper{units: []provider.Unit{
		{Sandbox: "x", Service: "db", Ref: "sbx-x-db", Paused: true},
	}}

	if err := Sleep(context.Background(), pausingSleeper{s}, "x"); err != nil {
		t.Fatal(err)
	}

	if got := s.stoppedSorted(); got != "sbx-x-db" {
		t.Errorf("a frozen service was left frozen: stopped %q", got)
	}

	if len(s.unpaused) != 1 {
		t.Errorf("the frozen service was stopped without being thawed first: unpaused %v", s.unpaused)
	}
}

// Independent services stop together. One after another, each waiting out docker's 10 s grace,
// made a 14-service stack take minutes to put to sleep.
func TestSleepStopsIndependentServicesInParallel(t *testing.T) {
	const delay = 300 * time.Millisecond

	p := &sleeper{delay: delay}
	for _, s := range []string{"a", "b", "c", "d"} {
		p.units = append(p.units, provider.Unit{Sandbox: "x", Service: s, Ref: "sbx-x-" + s, Running: true})
	}

	start := time.Now()

	if err := Sleep(context.Background(), p, "x"); err != nil {
		t.Fatal(err)
	}

	if took := time.Since(start); took > 2*delay {
		t.Errorf("four independent services took %s to sleep, want about one stop (%s)", took, delay)
	}
}

// A dependent stops before what it depends on, so an app is never left running against a
// database that has already gone.
func TestSleepStopsDependentsBeforeTheirDependencies(t *testing.T) {
	p := &sleeper{delay: 100 * time.Millisecond, units: []provider.Unit{
		{Sandbox: "x", Service: "db", Ref: "sbx-x-db", Running: true},
		{Sandbox: "x", Service: "api", Ref: "sbx-x-api", Running: true, DependsOn: []string{"db"}},
		{Sandbox: "x", Service: "web", Ref: "sbx-x-web", Running: true, DependsOn: []string{"api"}},
		{Sandbox: "x", Service: "worker", Ref: "sbx-x-worker", Running: true, DependsOn: []string{"db"}},
	}}

	if err := Sleep(context.Background(), p, "x"); err != nil {
		t.Fatal(err)
	}

	before := func(first, then string) {
		if !p.ended["sbx-x-"+first].Before(p.began["sbx-x-"+then]) {
			t.Errorf("%s began stopping before %s, which depends on it, had stopped", then, first)
		}
	}

	before("web", "api")
	before("api", "db")
	before("worker", "db")
}
