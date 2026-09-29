package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/slotlock"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// raceStub is a Provider that is safe to drive from several goroutines at once, as several
// sbx processes drive one docker engine. Create is slow on purpose, so two callers overlap in
// exactly the window a real `docker run` leaves between "checked it is unused" and "made it".
// Every container it makes gets a fresh Instance, as a docker container ID is.
type raceStub struct {
	provider.Provider

	mu      sync.Mutex
	units   map[string][]provider.Unit
	seq     int
	delay   time.Duration
	slots   []int // AllocSlot hands these out in order, then repeats the last
	allocs  int
	removed []string // sandboxes Remove()d
	unitsRm []string // refs RemoveUnit()d

	unservable provider.Endpoint // when set, created services are local and answer on this dead address

	createErr   func(service string, slot int) error // a `docker run` that fails after making the container
	execErr     func(ref string, argv []string) error
	probe       func(ref string) (bool, bool)
	volumes     map[string]bool
	createCalls []string
}

func newRaceStub() *raceStub {
	return &raceStub{units: map[string][]provider.Unit{}, volumes: map[string]bool{}}
}

func (s *raceStub) Name() string { return "stub" }

func (s *raceStub) AllocSlot(_ context.Context, sandbox string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if us := s.units[sandbox]; len(us) > 0 {
		return us[0].Slot, nil
	}

	if len(s.slots) == 0 {
		return 0, nil
	}

	i := min(s.allocs, len(s.slots)-1)
	s.allocs++

	return s.slots[i], nil
}

// Not loopback, so create's closing readiness line does not go looking for a daemon.
func (s *raceStub) Endpoints(_, _ string, slot, start int, ports []int) []provider.Endpoint {
	eps := make([]provider.Endpoint, 0, len(ports))
	for i := range ports {
		eps = append(eps, provider.Endpoint{Host: "sbx.test", Port: 20000 + slot*100 + start + i})
	}

	return eps
}

func (s *raceStub) Create(_ context.Context, sandbox string, slot, _ int, service string, svc spec.Service,
	_ []provider.Endpoint, _ string, _ provider.Isolation,
) error {
	time.Sleep(s.delay)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.createCalls = append(s.createCalls, fmt.Sprintf("%s/%s@%d", sandbox, service, slot))

	for i, u := range s.units[sandbox] {
		if u.Service != service {
			continue
		}

		if svc.Image == "" || svc.Image == u.Ref+":img" || !strings.HasPrefix(svc.Image, "new") {
			return nil // "already exists"
		}

		// A changed image replaces the container, as docker's Create does: same Ref, new
		// Instance, running.
		s.seq++
		s.units[sandbox][i].Instance = fmt.Sprintf("i%d", s.seq)
		s.units[sandbox][i].Running = true

		return nil
	}

	if svc.Volume != "" {
		s.volumes["sbx-"+sandbox+"-"+service+"-data"] = true
	}

	s.seq++
	s.units[sandbox] = append(s.units[sandbox], provider.Unit{
		Sandbox: sandbox, Service: service, Slot: slot, Ref: "sbx-" + sandbox + "-" + service,
		Instance: fmt.Sprintf("i%d", s.seq), Running: true,
	})

	if s.unservable.Port != 0 {
		u := &s.units[sandbox][len(s.units[sandbox])-1]
		u.Client = []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}
		u.Upstream = []provider.Endpoint{s.unservable}
	}

	if s.createErr != nil {
		return s.createErr(service, slot)
	}

	return nil
}

func (s *raceStub) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sandbox == "" {
		var all []provider.Unit
		for _, us := range s.units {
			all = append(all, us...)
		}

		return all, nil
	}

	return append([]provider.Unit(nil), s.units[sandbox]...), nil
}

func (s *raceStub) has(sandbox string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.units[sandbox]) > 0
}

func (s *raceStub) Probe(_ context.Context, ref string) (bool, bool) {
	if s.probe != nil {
		return s.probe(ref)
	}

	return true, true
}

func (s *raceStub) Healthy(context.Context, string) (bool, bool) { return true, true }

func (s *raceStub) Exec(_ context.Context, ref string, argv []string) (string, error) {
	if s.execErr != nil {
		return "", s.execErr(ref, argv)
	}

	return "", nil
}

func (s *raceStub) Logs(context.Context, string, int, bool, io.Writer) error { return nil }

func (s *raceStub) Stop(context.Context, string) error { return nil }

func (s *raceStub) Remove(_ context.Context, sandbox string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.units, sandbox)
	s.removed = append(s.removed, sandbox)

	return nil
}

func (s *raceStub) RemoveUnit(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.unitsRm = append(s.unitsRm, ref)

	for sb, us := range s.units {
		kept := us[:0]
		for _, u := range us {
			if u.Ref != ref {
				kept = append(kept, u)
			}
		}

		s.units[sb] = kept
		if len(kept) == 0 {
			delete(s.units, sb)
		}
	}

	return nil
}

func (s *raceStub) VolumeExists(_ context.Context, name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.volumes[name], nil
}

func (s *raceStub) CreateVolume(context.Context, string, map[string]string) error { return nil }
func (s *raceStub) RemoveVolume(context.Context, string) error                    { return nil }

func (s *raceStub) DataVolume(sandbox, service string) string {
	return "sbx-" + sandbox + "-" + service + "-data"
}

// captureOutput runs fn with stdout and stderr sent to one buffer.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w

	done := make(chan string)

	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()

	fn()

	_ = w.Close()

	return <-done
}

// ── N24: two `sbx with` for one name ─────────────────────────────────────────

// Two `sbx with qa-cc` started together: both saw the name unused, both created ("redis
// already exists" for the second), both ran, and the first to finish removed the sandbox
// while the other's command was still using it. One name is one fixture, so exactly one run
// may have it; the other is refused, and the sandbox is removed once, after its run.
func TestTwoWithRunsForOneNameNeverShareASandbox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.delay = 150 * time.Millisecond

	path := redisSpec(t)
	ran := filepath.Join(t.TempDir(), "ran")

	var (
		wg   sync.WaitGroup
		errs = make([]error, 2)
	)

	for i := range 2 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			errs[i] = With(context.Background(), p, path, "qa-cc", false, provider.IsolationContainer,
				5*time.Second, false, []string{"sh", "-c", "echo run >> " + ran + "; sleep 0.3"})
		}()
	}

	wg.Wait()

	body, _ := os.ReadFile(ran)
	if runs := strings.Count(string(body), "run"); runs != 1 {
		t.Errorf("%d runs used sandbox qa-cc at once, want exactly 1 (errors: %v, %v)", runs, errs[0], errs[1])
	}

	failed := 0

	for _, err := range errs {
		if err != nil {
			failed++

			if !strings.Contains(err.Error(), "qa-cc") {
				t.Errorf("the refusal does not name the sandbox: %v", err)
			}
		}
	}

	if failed != 1 {
		t.Errorf("want one run refused, got %d refusals: %v, %v", failed, errs[0], errs[1])
	}

	if len(p.removed) != 1 {
		t.Errorf("sandbox removed %d time(s), want once by its own run: %v", len(p.removed), p.removed)
	}
}

// `sbx with X` while a plain `sbx create X` is under way would see a half-made X - or none yet
// - and adopt it. It is refused at once instead of waiting and then refusing.
func TestWithRefusesANameAnotherCreateHolds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	release, err := slotlock.AcquireName(context.Background(), "busy", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	p := newRaceStub()
	began := time.Now()

	err = With(context.Background(), p, redisSpec(t), "busy", false, provider.IsolationContainer,
		time.Second, false, []string{"true"})
	if err == nil {
		t.Fatal("sbx with took a name another create holds")
	}

	if time.Since(began) > 2*time.Second {
		t.Errorf("waited %s before refusing", time.Since(began))
	}

	if len(p.createCalls) != 0 || len(p.removed) != 0 {
		t.Errorf("touched the other create's sandbox: creates=%v removed=%v", p.createCalls, p.removed)
	}
}

// Two `sbx create X` at once: the second waits for the first and then finds X made, rather
// than both creating it side by side.
func TestTwoCreatesForOneNameAreSerialised(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.delay = 100 * time.Millisecond

	var inside, worst int

	var mu sync.Mutex

	p.probe = func(string) (bool, bool) {
		mu.Lock()
		inside++
		worst = max(worst, inside)
		mu.Unlock()

		time.Sleep(400 * time.Millisecond)

		mu.Lock()
		inside--
		mu.Unlock()

		return true, true
	}

	path := redisSpec(t)

	var wg sync.WaitGroup

	for range 2 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := Create(context.Background(), p, path, "twice", false, provider.IsolationContainer, nil); err != nil {
				t.Error(err)
			}
		}()
	}

	wg.Wait()

	if us, _ := p.List(context.Background(), "twice"); len(us) != 1 {
		t.Errorf("want one redis, got %v", us)
	}

	// Only the first create health-checks; the second finds it already existing and running,
	// which is checked again but never at the same time as the first.
	if worst > 1 {
		t.Errorf("two creates of one sandbox were inside their health waits at once")
	}
}

// The teardown removes what this run created and nothing else. If something else appeared
// under the name while the command ran - an `sbx add`, another create - it is left, and said.
func TestWithTeardownRemovesOnlyWhatItCreated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.units["mix"] = []provider.Unit{
		{Sandbox: "mix", Service: "redis", Ref: "sbx-mix-redis", Instance: "mine", Running: true},
		{Sandbox: "mix", Service: "pg", Ref: "sbx-mix-pg", Instance: "theirs", Running: true},
	}

	var err error

	out := captureOutput(t, func() {
		err = removeOwned(context.Background(), p, "mix", map[string]bool{"mine": true})
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(p.removed) != 0 {
		t.Fatalf("removed the whole sandbox, including a service this run did not create")
	}

	if strings.Join(p.unitsRm, ",") != "sbx-mix-redis" {
		t.Errorf("removed %v, want only sbx-mix-redis", p.unitsRm)
	}

	for _, want := range []string{"pg", "sbx rm mix"} {
		if !strings.Contains(out, want) {
			t.Errorf("the teardown did not say %q about what it left:\n%s", want, out)
		}
	}

	// And all its own: the whole sandbox goes, volumes and all, as before.
	p.units["solo"] = []provider.Unit{{Sandbox: "solo", Service: "redis", Ref: "sbx-solo-redis", Instance: "a"}}

	_ = captureOutput(t, func() {
		err = removeOwned(context.Background(), p, "solo", map[string]bool{"a": true})
	})
	if err != nil || len(p.removed) != 1 || p.removed[0] != "solo" {
		t.Errorf("a sandbox that was all this run's was not removed whole: %v %v", err, p.removed)
	}
}

// ── N25: the slot lock ───────────────────────────────────────────────────────

// The slot lock is released once the first container exists - not after its health wait. A
// slow health check used to hold every other create on the machine until they gave up.
func TestSlotLockIsFreeDuringTheHealthWait(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	defer func(w time.Duration) { slotlock.SlotWait = w }(slotlock.SlotWait)
	slotlock.SlotWait = 200 * time.Millisecond

	p := newRaceStub()

	var free []bool

	p.probe = func(string) (bool, bool) {
		release, err := slotlock.Acquire(context.Background(), nil)
		free = append(free, err == nil)

		if err == nil {
			release()
		}

		return true, true
	}

	if err := Create(context.Background(), p, redisSpec(t), "slow", false, provider.IsolationContainer, nil); err != nil {
		t.Fatal(err)
	}

	if len(free) == 0 || !free[0] {
		t.Errorf("another create could not take the slot lock while this one waited for health: %v", free)
	}
}

// A slot lock still held when the wait runs out stops the create with the holder named. It
// used to go ahead unlocked, and waiters that timed out together took one slot between them.
func TestCreateStopsWhenTheSlotLockWaitRunsOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	defer func(w time.Duration) { slotlock.SlotWait = w }(slotlock.SlotWait)
	slotlock.SlotWait = 200 * time.Millisecond

	lock, _ := slotlock.Path()
	_ = os.MkdirAll(filepath.Dir(lock), 0o755)

	if err := os.WriteFile(lock, []byte(fmt.Sprint(os.Getppid())), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newRaceStub()

	err := Create(context.Background(), p, redisSpec(t), "blocked", false, provider.IsolationContainer, nil)
	if err == nil {
		t.Fatal("create went ahead without the slot lock")
	}

	if !strings.Contains(err.Error(), fmt.Sprint(os.Getppid())) {
		t.Errorf("the error does not name the holder: %v", err)
	}

	if p.allocs != 0 || len(p.createCalls) != 0 {
		t.Errorf("allocated or created without the lock: allocs=%d creates=%v", p.allocs, p.createCalls)
	}
}

// ── N27: signals ─────────────────────────────────────────────────────────────

// SIGTERM or Ctrl-C to `sbx with` used to kill it outright (no handler), leaving the sandbox.
// Cancelled by a signal, it stops the command, removes the sandbox, and exits 128+signal.
func TestWithTearsDownWhenSignalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()

	ctx, cancel := context.WithCancelCause(context.Background())

	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel(&Interrupted{Signal: syscall.SIGTERM})
	}()

	began := time.Now()

	err := With(ctx, p, redisSpec(t), "sig", false, provider.IsolationContainer, 5*time.Second, false,
		[]string{"sleep", "30"})

	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("the signal did not stop the command: took %s", took)
	}

	var st interface{ ChildStatus() int }
	if !errors.As(err, &st) || st.ChildStatus() != 143 {
		t.Errorf("want exit status 143 for SIGTERM, got %v", err)
	}

	if len(p.removed) != 1 {
		t.Errorf("the sandbox was not removed after the signal: %v", p.removed)
	}
}

// ── N12: a failed `docker run` ──────────────────────────────────────────────

// A `docker run` that fails after making its container ("port is already allocated") left it
// in Created, and wake then reported it serving with no network. It is removed, and for a new
// sandbox the create moves to the next free slot once, since the slot was probed free and lost
// a race.
func TestAFailedRunIsRemovedAndRetriedOnTheNextSlot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.slots = []int{2, 3}
	p.createErr = func(_ string, slot int) error {
		if slot == 2 {
			return errors.New("docker run: Bind for 127.0.0.1:30360 failed: port is already allocated")
		}

		return nil
	}

	var err error

	out := captureOutput(t, func() {
		err = Create(context.Background(), p, redisSpec(t), "raced", false, provider.IsolationContainer, nil)
	})
	if err != nil {
		t.Fatalf("the retry on the next slot failed: %v\n%s", err, out)
	}

	if strings.Join(p.unitsRm, ",") != "sbx-raced-redis" {
		t.Errorf("the container the failed run made was not removed: %v", p.unitsRm)
	}

	us, _ := p.List(context.Background(), "raced")
	if len(us) != 1 || us[0].Slot != 3 {
		t.Errorf("want one redis on slot 3, got %v", us)
	}
}

// When the next slot is the same one, there is nothing to retry: the leftover is removed and
// the error says to re-run.
func TestAFailedRunWithNoOtherSlotSaysToRerun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.slots = []int{2}
	p.createErr = func(string, int) error {
		return errors.New("Bind for 127.0.0.1:30360 failed: port is already allocated")
	}

	var err error

	_ = captureOutput(t, func() {
		err = Create(context.Background(), p, redisSpec(t), "raced", false, provider.IsolationContainer, nil)
	})
	if err == nil {
		t.Fatal("a failed run reported success")
	}

	if !strings.Contains(err.Error(), "sbx create raced") {
		t.Errorf("the error does not say to re-run: %v", err)
	}

	if p.has("raced") {
		t.Error("the container the failed run made was left behind")
	}
}

// Any failed run, not just a port clash, takes its half-made container with it - but only one
// this create made: an existing service is never removed for a failed re-create.
func TestAFailedRunOnlyRemovesAContainerThisCreateMade(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.createErr = func(string, int) error { return errors.New("docker: invalid reference format") }

	var err error

	_ = captureOutput(t, func() {
		err = Create(context.Background(), p, redisSpec(t), "bad", false, provider.IsolationContainer, nil)
	})
	if err == nil || p.has("bad") {
		t.Fatalf("want the failure and no leftover: err=%v units=%v", err, p.units["bad"])
	}

	// An existing, healthy service whose re-create reports an error is not this create's.
	p.createErr = nil
	p.units["keep"] = []provider.Unit{{Sandbox: "keep", Service: "redis", Ref: "sbx-keep-redis", Instance: "old", Running: true}}
	p.createErr = func(string, int) error { return errors.New("transient") }

	_ = captureOutput(t, func() {
		_ = Create(context.Background(), p, redisSpec(t), "keep", false, provider.IsolationContainer, nil)
	})

	if !p.has("keep") {
		t.Error("an existing service was removed because a re-create reported an error")
	}
}

// ── N18: a rebuild while asleep ──────────────────────────────────────────────

// Asleep before the create is not asleep after it when the create replaced the container: a
// `build` service whose context changed is recreated, running, and needs its health wait and
// init like any new one. It used to print "asleep - left as it is" with init never run.
func TestARecreatedServiceIsNotSkippedAsAsleep(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.units["zz"] = []provider.Unit{{Sandbox: "zz", Service: "web", Ref: "sbx-zz-web", Instance: "old", Running: false}}

	var inits []string

	p.execErr = func(_ string, argv []string) error {
		inits = append(inits, strings.Join(argv, " "))
		return nil
	}

	svc := spec.Service{Image: "new-build-abc", Ports: []int{80}, Health: "true", Init: []string{"echo seeded"}}

	var err error

	out := captureOutput(t, func() {
		err = createOne(context.Background(), p, "zz", 0, 0, "web", svc, ".", provider.IsolationContainer)
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out, "asleep") {
		t.Errorf("a recreated service was reported as asleep:\n%s", out)
	}

	if !slicesContain(inits, "sh -c echo seeded") {
		t.Errorf("the recreated service's init did not run: %v", inits)
	}
}

func slicesContain(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}

	return false
}

// ── N21: what a failed mount check leaves ────────────────────────────────────

// good, m (a file that mounts as a directory), z: z is never created, and the output has to
// say so, along with what was kept and what was removed.
func TestAFailedMountSaysWhatWasKeptRemovedAndNotAttempted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	file := filepath.Join(dir, "conf.xml")

	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := writeSpec(t, `{"version": 1, "services": {
		"good": {"image": "redis:7-alpine", "ports": [6379]},
		"m":    {"image": "redis:7-alpine", "ports": [6380], "depends_on": ["good"], "files": {"`+file+`": "/etc/conf.xml"}},
		"z":    {"image": "redis:7-alpine", "ports": [6381], "depends_on": ["m"]}}}`)

	p := newRaceStub()
	p.execErr = func(ref string, argv []string) error {
		if ref == "sbx-three-m" && argv[0] == "test" {
			return errors.New("exit status 1")
		}

		return nil
	}

	err := Create(context.Background(), p, path, "three", false, provider.IsolationContainer, nil)
	if err == nil {
		t.Fatal("a broken mount was reported as created")
	}

	for _, want := range []string{"Kept: good", "Not attempted: z", "removed sbx-three-m"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}

	// One service and nothing else: there is no "rest" to keep.
	single := writeSpec(t, `{"version": 1, "services": {
		"m": {"image": "redis:7-alpine", "ports": [6380], "files": {"`+file+`": "/etc/conf.xml"}}}}`)

	p2 := newRaceStub()
	p2.execErr = func(string, []string) error { return errors.New("exit status 1") }

	err = Create(context.Background(), p2, single, "one", false, provider.IsolationContainer, nil)
	if err == nil {
		t.Fatal("a broken mount was reported as created")
	}

	if strings.Contains(err.Error(), "rest of the sandbox") || strings.Contains(err.Error(), "Kept:") {
		t.Errorf("a one-service sandbox was told the rest is kept:\n%v", err)
	}
}

// ── N7: a leftover volume ────────────────────────────────────────────────────

// A new sandbox whose service's data volume already exists - an orphan from an earlier sandbox
// of the same name, or a failed fork - silently started on that old data. It still does (the
// data may be wanted), but says so and how to start clean.
func TestANewSandboxWarnsWhenItAdoptsALeftoverVolume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	path := writeSpec(t, `{"version": 1, "services": {"redis": {"image": "redis:7-alpine", "ports": [6379], "volume": "/data"}}}`)

	p := newRaceStub()
	p.volumes["sbx-old-redis-data"] = true

	var err error

	out := captureOutput(t, func() {
		err = Create(context.Background(), p, path, "old", false, provider.IsolationContainer, nil)
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"sbx-old-redis-data", "already existed", "docker volume rm sbx-old-redis-data"} {
		if !strings.Contains(out, want) {
			t.Errorf("no warning saying %q:\n%s", want, out)
		}
	}

	// Its own volume on a re-run is not a leftover.
	out = captureOutput(t, func() {
		err = Create(context.Background(), p, path, "old", false, provider.IsolationContainer, nil)
	})
	if err != nil || strings.Contains(out, "already existed") {
		t.Errorf("a re-run over the sandbox's own volume warned: %v\n%s", err, out)
	}
}

// ── minor ────────────────────────────────────────────────────────────────────

// --keep used to print nothing, so the kept sandbox was easy to forget.
func TestWithKeepSaysItKeptTheSandbox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()

	var err error

	out := captureOutput(t, func() {
		err = With(context.Background(), p, redisSpec(t), "kept", false, provider.IsolationContainer,
			5*time.Second, true, []string{"true"})
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, "kept") || !strings.Contains(out, "sbx rm kept") {
		t.Errorf("--keep did not say it kept the sandbox and how to remove it:\n%s", out)
	}
}

// `sbx add` with a name already in the sandbox refuses first, before any warning about the
// flags of a service it is not going to create.
func TestAddRefusesADuplicateBeforeWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.units["s"] = []provider.Unit{{Sandbox: "s", Service: "redis", Ref: "sbx-s-redis", Instance: "a"}}

	var err error

	out := captureOutput(t, func() {
		err = Add(context.Background(), p, redisSpec(t), "s", "redis", "redis:7-alpine", []int{6379}, "",
			nil, "", nil, provider.IsolationContainer)
	})
	if err == nil || !strings.Contains(err.Error(), "already has a service") {
		t.Fatalf("want the duplicate refused, got %v", err)
	}

	if strings.Contains(out, "health") {
		t.Errorf("warned about --health for a service it refused to add:\n%s", out)
	}
}

// GuestDialer: with unservable set, every created service answers on no port - the shape of a
// workload whose health check passes inside the container but which nothing outside can reach.
func (s *raceStub) GuestDialer(string, string, int) (provider.DialFunc, bool) {
	if s.unservable.Port == 0 {
		return nil, false
	}

	addr := s.unservable.String()

	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}, true
}
