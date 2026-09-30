package cli

// Two readiness verdict rules, found in the RC4 isolation retest.
//
// No line claims success for a service the same command then fails: create printed
// "redis ✓ 127.0.0.1:…" when its health check passed and then, in its final check, "nothing
// listens" or "no network interface but loopback" about the same service; `sbx wake` printed
// "redis serving" before an exit 1. On a skim both read as success.
//
// A verdict that waiting cannot change fails at once: a container with no network interface but
// loopback, or one that started during this command and has exited since, was waited on for the
// whole timeout. One that is still binding its port is waited on, as before.

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// loopbackOnly is a redis container whose guest has no network: a health check inside it passes,
// and nothing outside can reach it (Kata on a nested host).
func loopbackOnly(t *testing.T) *insideStub {
	p := serving(t)
	p.dev = devHeader + devLoop

	return p
}

func TestWakePrintsNoServingLineForAServiceItThenFails(t *testing.T) {
	p := loopbackOnly(t)

	var err error

	out := captureOutput(t, func() { err = Ready(context.Background(), p, "x", 600*time.Millisecond) })

	if err == nil {
		t.Fatal("ready passed a container with no network interface but loopback")
	}

	if strings.Contains(out, "serving") {
		t.Errorf("printed a serving line, then failed:\n%s\nerror: %v", out, err)
	}
}

// The success case keeps its lines: one per service, then the sandbox.
func TestWakeStillPrintsServingLinesWhenItPasses(t *testing.T) {
	var err error

	out := captureOutput(t, func() { err = Ready(context.Background(), serving(t), "x", 2*time.Second) })

	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"redis", "serving", `sandbox "x" is serving`} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not say %q:\n%s", want, out)
		}
	}
}

// insideCreateStub is insideStub that create can make a service on.
type insideCreateStub struct{ *insideStub }

func (s insideCreateStub) Endpoints(_, _ string, _, _ int, ports []int) []provider.Endpoint {
	eps := make([]provider.Endpoint, 0, len(ports))
	for _, p := range ports {
		eps = append(eps, provider.Endpoint{Host: "127.0.0.1", Port: 20000 + p})
	}

	return eps
}

func (insideCreateStub) Create(context.Context, string, int, int, string, spec.Service,
	[]provider.Endpoint, string, provider.Isolation,
) error {
	return nil
}

func TestCreateDoesNotTickAServiceThatDoesNotServe(t *testing.T) {
	old := createServeWait
	createServeWait = time.Second

	t.Cleanup(func() { createServeWait = old })

	p := insideCreateStub{loopbackOnly(t)}

	var err error

	out := captureOutput(t, func() {
		err = createOne(context.Background(), p, "x", 0, 0, "redis",
			spec.Service{Ports: []int{6379}, Health: "true"}, ".", provider.IsolationContainer)
	})

	if strings.Contains(out, "✓") {
		t.Errorf("ticked a service it then found not serving:\n%s", out)
	}

	if !strings.Contains(out, "✗") || !strings.Contains(out, "redis") {
		t.Errorf("the service's line does not say it failed:\n%s", out)
	}

	if err == nil || !strings.Contains(err.Error(), "no network interface but loopback") {
		t.Fatalf("want the loopback verdict, got %v", err)
	}
}

// The success case of create keeps its one line per service.
func TestCreateStillTicksAServiceThatServes(t *testing.T) {
	p := insideCreateStub{serving(t)}

	var err error

	out := captureOutput(t, func() {
		err = createOne(context.Background(), p, "x", 0, 0, "redis",
			spec.Service{Ports: []int{6379}, Health: "true"}, ".", provider.IsolationContainer)
	})

	if err != nil || !strings.Contains(out, "✓") || strings.Contains(out, "✗") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}

	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 1 {
		t.Errorf("a serving service printed %d lines, want 1:\n%s", n, out)
	}
}

func TestReadyFailsFastWhenTheContainerHasOnlyLoopback(t *testing.T) {
	p := loopbackOnly(t)

	start := time.Now()
	err := Ready(context.Background(), p, "x", 10*time.Second)

	if err == nil || !strings.Contains(err.Error(), "no network interface but loopback") {
		t.Fatalf("want the loopback verdict, got %v", err)
	}

	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("waited %s of a 10s timeout on a verdict waiting cannot change", took.Round(time.Millisecond))
	}
}

// diesStub is one local service whose container this command started and which has exited since,
// with or without a health check.
type diesStub struct {
	provider.Provider

	daemon  int
	health  bool
	fresh   bool // absent until Create makes it
	created bool
}

func (s *diesStub) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	if s.fresh && !s.created {
		return nil, nil
	}

	return []provider.Unit{{
		Sandbox: sandbox, Service: "web", Ref: "sbx-x-web", Running: false,
		Client:   []provider.Endpoint{{Host: "127.0.0.1", Port: s.daemon}},
		Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}},
		Private:  []int{8080},
	}}, nil
}

func (s *diesStub) Healthy(context.Context, string) (bool, bool) { return false, s.health }
func (s *diesStub) Probe(context.Context, string) (bool, bool)   { return false, s.health }

func (s *diesStub) ExitOf(context.Context, string) (provider.ExitState, error) {
	// Started a moment ago - during whichever command is asking - and exited since.
	return provider.ExitState{Status: "exited", ExitCode: 1, StartedAt: time.Now().Add(-time.Millisecond)}, nil
}

func (s *diesStub) Exec(context.Context, string, []string) (string, error) { return "", nil }

func (s *diesStub) Logs(context.Context, string, int, bool, io.Writer) error {
	return nil
}

func (s *diesStub) Endpoints(_, _ string, _, _ int, ports []int) []provider.Endpoint {
	return []provider.Endpoint{{Host: "127.0.0.1", Port: 20000 + ports[0]}}
}

func (s *diesStub) Create(context.Context, string, int, int, string, spec.Service,
	[]provider.Endpoint, string, provider.Isolation,
) error {
	s.created = true
	return nil
}

func TestReadyFailsFastWhenTheContainerExited(t *testing.T) {
	for _, health := range []bool{true, false} {
		name := "without a health check"
		if health {
			name = "with a health check"
		}

		t.Run(name, func(t *testing.T) {
			p := &diesStub{daemon: holding(t), health: health}

			start := time.Now()
			err := Ready(context.Background(), p, "x", 10*time.Second)

			if err == nil || !strings.Contains(err.Error(), "exit code 1") {
				t.Fatalf("want the exit verdict, got %v", err)
			}

			if took := time.Since(start); took > 3*time.Second {
				t.Errorf("waited %s of a 10s timeout on a container that has exited", took.Round(time.Millisecond))
			}
		})
	}
}

// Create's health wait stops at a container that exited during it rather than waiting out the
// health timeout (two minutes by default) to report "never became ready".
func TestCreateHealthWaitFailsFastWhenTheContainerExited(t *testing.T) {
	p := &diesStub{daemon: holding(t), health: true, fresh: true}

	start := time.Now()

	var err error

	out := captureOutput(t, func() {
		err = createOneWithin(context.Background(), p, "x", 0, 0, "web",
			spec.Service{Ports: []int{8080}, Health: "true"}, ".", provider.IsolationContainer, 10*time.Second, nil)
	})

	if err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("want the exit verdict, got %v\n%s", err, out)
	}

	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("waited %s of a 10s health timeout on a container that has exited", took.Round(time.Millisecond))
	}
}

// bindsLater is a serving container whose process binds its port only after a while: redis behind
// `sleep 8; exec redis-server`. Before then it has no listener, or (localOnly) a listener on
// 127.0.0.1 only - the shape of an image's entrypoint running its init against a private server.
type bindsLater struct {
	*insideStub

	at        time.Time
	localOnly bool
}

func (s bindsLater) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	if len(argv) == 2 && argv[1] == "/proc/net/tcp" && time.Now().Before(s.at) {
		if s.localOnly {
			return tcpHeader + tcpLocal6379, nil
		}

		return tcpHeader, nil
	}

	return s.insideStub.Exec(ctx, ref, argv)
}

func TestReadyWaitsForASlowBind(t *testing.T) {
	for _, localOnly := range []bool{false, true} {
		name := "nothing listens yet"
		if localOnly {
			name = "listens on 127.0.0.1 only for now"
		}

		t.Run(name, func(t *testing.T) {
			p := bindsLater{insideStub: serving(t), at: time.Now().Add(time.Second), localOnly: localOnly}

			if err := Ready(context.Background(), p, "x", 5*time.Second); err != nil {
				t.Fatalf("a service that binds after 1s failed a 5s ready: %v", err)
			}

			p = bindsLater{insideStub: serving(t), at: time.Now().Add(time.Hour), localOnly: localOnly}

			start := time.Now()
			if err := Ready(context.Background(), p, "x", 700*time.Millisecond); err == nil {
				t.Fatal("a service that never binds passed")
			}

			if took := time.Since(start); took < 700*time.Millisecond {
				t.Errorf("gave up after %s on a verdict that waiting can change", took.Round(time.Millisecond))
			}
		})
	}
}
