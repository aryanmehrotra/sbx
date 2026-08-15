package provider

import (
	"strings"
	"testing"
)

func TestParseDockerHost(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		network string
		address string
		fails   bool
	}{
		{name: "bare path", in: "/var/run/docker.sock", network: "unix", address: "/var/run/docker.sock"},
		{name: "unix url", in: "unix:///var/run/docker.sock", network: "unix", address: "/var/run/docker.sock"},
		{name: "colima", in: "unix:///Users/x/.colima/default/docker.sock", network: "unix", address: "/Users/x/.colima/default/docker.sock"},
		{name: "tcp", in: "tcp://127.0.0.1:2375", network: "tcp", address: "127.0.0.1:2375"},
		{name: "http", in: "http://192.168.1.9:2375", network: "tcp", address: "192.168.1.9:2375"},
		{name: "spaces trimmed", in: "  /var/run/docker.sock\n", network: "unix", address: "/var/run/docker.sock"},

		// Both of these have to fail loudly. A TLS endpoint silently dialled without client
		// certificates, or a named pipe treated as a file path, would surface much later as
		// something that looks like a broken sandbox rather than a wrong address.
		{name: "tls refused", in: "https://docker.example.com:2376", fails: true},
		{name: "npipe refused", in: "npipe:////./pipe/docker_engine", fails: true},
		{name: "unknown scheme", in: "ssh://host/docker.sock", fails: true},
		{name: "empty", in: "", fails: true},
		{name: "blank", in: "   ", fails: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseDockerHost(c.in)

			if c.fails {
				if err == nil {
					t.Fatalf("parseDockerHost(%q) = %v, want error", c.in, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseDockerHost(%q): %v", c.in, err)
			}

			if got.Network != c.network || got.Address != c.address {
				t.Errorf("parseDockerHost(%q) = %s://%s, want %s://%s",
					c.in, got.Network, got.Address, c.network, c.address)
			}
		})
	}
}

// The Windows message has to say what to do, not just what went wrong: "cannot dial a named
// pipe" leaves someone stuck, "run under WSL2" does not.
func TestNamedPipeErrorSuggestsWSL(t *testing.T) {
	_, err := parseDockerHost("npipe:////./pipe/docker_engine")
	if err == nil {
		t.Fatal("expected an error for a named pipe")
	}

	if !strings.Contains(err.Error(), "WSL2") {
		t.Errorf("error %q does not tell the reader what to do instead", err)
	}
}
