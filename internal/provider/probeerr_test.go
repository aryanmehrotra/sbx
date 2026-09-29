package provider

// "Could not ask" is not "nothing to check".
//
// Probe and Healthy return declared=false to mean the workload declares no health check, and
// every caller reads that as "nothing to wait for": the CLI's wait returns at once, the daemon
// marks the unit awake. Both backends also returned it when the question itself failed - a
// docker inspect that errored, a kubectl that could not reach the API server. Measured on
// docker: the Engine API stalled for about a minute during a Kata start, every inspect failed,
// and `sbx ready` reported "serving" for a service whose container had exited.

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// probeEngine serves the Engine API on a unix socket with one handler for every path.
func probeEngine(t *testing.T, h http.HandlerFunc) *dockerProvider {
	t.Helper()

	// Not t.TempDir(): on macOS that path is longer than a unix socket name may be.
	dir, err := os.MkdirTemp("/tmp", "sbx-probe")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "d.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	return newDocker(dockerEndpoint{Network: "unix", Address: sock})
}

func TestDockerProbeThatCannotAskIsNotUndeclared(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}

	d := probeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "engine busy", http.StatusInternalServerError)
	})

	if serving, declared := d.Probe(context.Background(), "sbx-x-redis"); serving || !declared {
		t.Errorf("Probe with a failing inspect = (serving %v, declared %v), want (false, true): "+
			"an error read as \"no health check\" is reported as serving", serving, declared)
	}

	if serving, declared := d.Healthy(context.Background(), "sbx-x-redis"); serving || !declared {
		t.Errorf("Healthy with a failing inspect = (serving %v, declared %v), want (false, true)",
			serving, declared)
	}
}

// The other half: a container that genuinely declares nothing still says so.
func TestDockerProbeOfAContainerWithNoHealthCheckIsUndeclared(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}

	d := probeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Config": {}, "State": {"Status": "running"}}`))
	})

	if serving, declared := d.Probe(context.Background(), "sbx-x-web"); serving || declared {
		t.Errorf("Probe of a container with no HEALTHCHECK = (%v, %v), want (false, false)", serving, declared)
	}

	if serving, declared := d.Healthy(context.Background(), "sbx-x-web"); serving || declared {
		t.Errorf("Healthy of a container with no HEALTHCHECK = (%v, %v), want (false, false)", serving, declared)
	}
}

func TestKubernetesProbeThatCannotAskIsNotUndeclared(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script as kubectl")
	}

	bin := t.TempDir()
	script := "#!/bin/sh\necho 'Unable to connect to the server: dial tcp: i/o timeout' >&2\nexit 1\n"

	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin)

	k := &kubeProvider{namespace: "sbx", ready: map[string]readyEntry{}}

	if serving, declared := k.Probe(context.Background(), "sbx-x-redis"); serving || !declared {
		t.Errorf("Probe with an unreachable API server = (%v, %v), want (false, true)", serving, declared)
	}

	if serving, declared := k.Healthy(context.Background(), "sbx-x-redis"); serving || !declared {
		t.Errorf("Healthy with an unreachable API server = (%v, %v), want (false, true)", serving, declared)
	}
}
