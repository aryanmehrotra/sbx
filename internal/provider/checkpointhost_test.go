package provider

import (
	"errors"
	"testing"
)

// The macOS refusal is about where the ENGINE runs, not where the CLI runs. A Mac driving a
// Linux daemon over tcp:// has a real Linux kernel under its containers, so it goes on to the
// daemon's own experimental/CRIU checks; a socket or a loopback port on a Mac is its local VM.
func TestCheckpointRefusalKeysOffTheEngineNotTheCLI(t *testing.T) {
	old := hostOS
	t.Cleanup(func() { hostOS = old })

	cases := []struct {
		os      string
		ep      dockerEndpoint
		refused bool
	}{
		{"darwin", dockerEndpoint{Network: "unix", Address: "/Users/x/.colima/default/docker.sock"}, true},
		{"darwin", dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"}, true},
		{"darwin", dockerEndpoint{Network: "tcp", Address: "127.0.0.1:2375"}, true},
		{"darwin", dockerEndpoint{Network: "tcp", Address: "localhost:2375"}, true},
		{"darwin", dockerEndpoint{Network: "tcp", Address: "[::1]:2375"}, true},
		{"darwin", dockerEndpoint{Network: "tcp", Address: "10.0.0.5:2375"}, false},
		{"darwin", dockerEndpoint{Network: "tcp", Address: "build-box.internal:2375"}, false},
		{"linux", dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"}, false},
	}

	for _, c := range cases {
		hostOS = c.os

		err := checkpointHost(c.ep)
		if got := errors.Is(err, ErrCheckpointNeedsLinux); got != c.refused {
			t.Errorf("%s CLI, engine %s: refused=%v, want %v (%v)", c.os, c.ep, got, c.refused, err)
		}
	}
}

// And through the provider: a remote endpoint is not refused by the host check, so the error
// (if any) comes from asking that daemon - not the macOS refusal.
func TestCheckpointReadyAsksARemoteDaemon(t *testing.T) {
	old := hostOS
	hostOS = "darwin"

	t.Cleanup(func() { hostOS = old })

	local := &dockerProvider{endpoint: dockerEndpoint{Network: "unix", Address: "/nonexistent/docker.sock"}}
	if err := local.checkpointReady(); !errors.Is(err, ErrCheckpointNeedsLinux) {
		t.Fatalf("a local engine on macOS was not refused: %v", err)
	}

	if err := checkpointHost(dockerEndpoint{Network: "tcp", Address: "10.0.0.5:2375"}); err != nil {
		t.Fatalf("a remote Linux engine was refused by the Mac's OS: %v", err)
	}
}
