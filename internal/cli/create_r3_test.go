package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/slotlock"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// ── R2: a `sbx with` owns its name for its whole run ────────────────────────

// `with` let go of the name once its create finished, so an `sbx create X` during the command
// reused the ephemeral sandbox, printed "ready", and then `with` removed it. Create and add of a
// name a live `with` owns are refused at once, and say why.
func TestACreateOrAddDuringAWithIsRefusedAtOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	path := redisSpec(t)
	started := filepath.Join(t.TempDir(), "started")

	done := make(chan error, 1)

	go func() {
		done <- With(context.Background(), p, path, "eph", false, provider.IsolationContainer, 5*time.Second,
			false, []string{"sh", "-c", "touch " + started + "; sleep 1"})
	}()

	for i := 0; ; i++ {
		if _, err := os.Stat(started); err == nil {
			break
		}

		if i > 200 {
			t.Fatal("the with command never started")
		}

		time.Sleep(25 * time.Millisecond)
	}

	began := time.Now()
	createErr := Create(context.Background(), p, path, "eph", false, provider.IsolationContainer, nil)
	addErr := Add(context.Background(), p, path, "eph", "extra", "redis:7-alpine", []int{6380}, "true",
		nil, "", nil, provider.IsolationContainer)

	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("waited %s; a name a live sbx with owns must be refused at once", took)
	}

	for what, err := range map[string]error{"create": createErr, "add": addErr} {
		if err == nil {
			t.Errorf("%s used the ephemeral sandbox of a running sbx with", what)
			continue
		}

		for _, want := range []string{"ephemeral sandbox of `sbx with`", "use another name", "removed when that command ends"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s's refusal does not say %q: %v", what, want, err)
			}
		}
	}

	if err := <-done; err != nil {
		t.Fatalf("the with run failed: %v", err)
	}

	if len(p.removed) != 1 || p.has("eph") {
		t.Errorf("the ephemeral sandbox was not removed exactly once: %v", p.removed)
	}
}

// ── minor: messages ──────────────────────────────────────────────────────────

// The refusal wrapped a multi-line lock error in "(...)", which read as an unclosed bracket.
func TestWithsRefusalOfAHeldNameReadsCleanly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	release, err := slotlock.AcquireName(context.Background(), "held", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	err = With(context.Background(), newRaceStub(), redisSpec(t), "held", false, provider.IsolationContainer,
		time.Second, false, []string{"true"})
	if err == nil {
		t.Fatal("with took a held name")
	}

	if o, c := strings.Count(err.Error(), "("), strings.Count(err.Error(), ")"); o != c {
		t.Errorf("unbalanced brackets (%d open, %d close):\n%v", o, c, err)
	}

	if strings.Contains(err.Error(), "(sandbox") {
		t.Errorf("the lock error is still wrapped in brackets:\n%v", err)
	}
}

// After `sbx with` removed a sandbox whose workload never served, the error still said "The
// sandbox was created; `sbx rm X` removes it" - advice to remove something already gone.
func TestWithDoesNotAdviseRemovingASandboxItRemoved(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	defer func(w time.Duration) { createServeWait = w }(createServeWait)
	createServeWait = 300 * time.Millisecond

	p := newRaceStub()
	p.unservable = acceptThenClose(t)

	var err error

	_ = captureOutput(t, func() {
		err = With(context.Background(), p, redisSpec(t), "dud", false, provider.IsolationContainer,
			5*time.Second, false, []string{"true"})
	})
	if err == nil {
		t.Fatal("a workload that never served was reported ready")
	}

	if strings.Contains(err.Error(), "removes it") {
		t.Errorf("advised removing a sandbox sbx with already removed:\n%v", err)
	}

	if !strings.Contains(err.Error(), "removed") || p.has("dud") {
		t.Errorf("want the sandbox removed and the error to say so: units=%v\n%v", p.units["dud"], err)
	}

	// A plain create keeps the advice: there, the sandbox is still there.
	p2 := newRaceStub()
	p2.unservable = acceptThenClose(t)

	_ = captureOutput(t, func() {
		err = Create(context.Background(), p2, redisSpec(t), "dud2", false, provider.IsolationContainer, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "sbx rm dud2") {
		t.Errorf("a plain create lost the advice to remove what it left: %v", err)
	}
}

// ── N31: remember the spec as soon as a container exists ─────────────────────

// A create that failed after making containers (a readiness refusal, a broken mount on its
// second service) never recorded its spec, so `sbx env <sandbox>` from another directory said
// "no sandbox.json here", and from a directory with a different sandbox.json read the wrong one.
func TestAPartialCreateStillRecordsItsSpec(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	file := filepath.Join(dir, "conf.xml")

	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := writeSpec(t, `{"version": 1, "services": {
		"good": {"image": "redis:7-alpine", "ports": [6379]},
		"m":    {"image": "redis:7-alpine", "ports": [6380], "depends_on": ["good"], "files": {"`+file+`": "/etc/conf.xml"}}}}`)

	p := newRaceStub()
	p.execErr = func(ref string, argv []string) error {
		if ref == "sbx-half-m" && argv[0] == "test" {
			return errors.New("exit status 1")
		}

		return nil
	}

	placed := 0

	err := Create(context.Background(), p, path, "half", false, provider.IsolationContainer,
		func() { placed++; Remember("half", "", path) })
	if err == nil {
		t.Fatal("the broken mount was reported as created")
	}

	if placed != 1 {
		t.Errorf("the placed hook ran %d times, want once, when the first container existed", placed)
	}

	o, ok := Recall("half")
	if !ok || o.Spec != path {
		t.Errorf("Recall(half) = %+v, %v; want the spec %s", o, ok, path)
	}

	// Nothing made, nothing recorded.
	p2 := newRaceStub()
	p2.createErr = func(string, int) error { return errors.New("docker: invalid reference format") }

	_ = captureOutput(t, func() {
		_ = Create(context.Background(), p2, redisSpec(t), "none", false, provider.IsolationContainer,
			func() { Remember("none", "", "x.json") })
	})

	if _, ok := Recall("none"); ok {
		t.Error("a create that left nothing recorded a spec")
	}
}

// ── R5: a port another service of the same sandbox holds ─────────────────────

// Re-creating a sandbox from a different spec could hand a new service the port an old service
// of the same sandbox still held, and docker then failed with "port is already allocated". It is
// found before docker run, and the error says which service holds it and what to do.
func TestANewServiceIsRefusedAPortItsOwnSandboxHolds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := newRaceStub()
	p.units["x"] = []provider.Unit{{Sandbox: "x", Service: "web", Slot: 1, Ref: "sbx-x-web", Instance: "a",
		Running: true, Client: []provider.Endpoint{{Host: "sbx.test", Port: 20100}}}}

	svc := spec.Service{Image: "redis:7-alpine", Ports: []int{6379}}

	err := createOne(context.Background(), p, "x", 1, 0, "redis", svc, ".", provider.IsolationContainer)
	if err == nil {
		t.Fatal("a service was given a port another service of its sandbox holds")
	}

	for _, want := range []string{`"redis"`, "20100", "web", "sbx rm x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}

	if len(p.createCalls) != 0 {
		t.Errorf("docker run was attempted anyway: %v", p.createCalls)
	}

	// Re-creating web itself, on its own port, is not a clash.
	if err := createOne(context.Background(), p, "x", 1, 0, "web", svc, ".", provider.IsolationContainer); err != nil {
		t.Errorf("a service's own port was reported as held: %v", err)
	}
}

// A fork fills its data volumes from the snapshot and then creates, so the volumes are there
// before the sandbox by design. The leftover-volume warning must not fire for it.
func TestAForkIsNotWarnedAboutTheVolumesItRestored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	path := writeSpec(t, `{"version": 1, "services": {"redis": {"image": "redis:7-alpine", "ports": [6379], "volume": "/data"}}}`)

	p := newRaceStub()
	p.volumes["sbx-fk-redis-data"] = true

	var err error

	out := captureOutput(t, func() {
		err = createLocked(context.Background(), p, path, "fk", false, provider.IsolationContainer,
			createOpts{healthTimeout: time.Second, volumesRestored: true})
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out, "already existed") {
		t.Errorf("a fork was warned about the volume it had just restored:\n%s", out)
	}
}
