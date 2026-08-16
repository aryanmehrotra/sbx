package provider

// Finding the Docker daemon without assuming which machine you are on.
//
// The first version defaulted to ~/.colima/default/docker.sock, which is one person's Mac.
// Everywhere else that path does not exist: Linux puts the socket in /var/run, Docker
// Desktop and Rancher put it under the home directory, and a remote daemon is a TCP address
// with no socket at all. Hardcoding any one of them makes the tool a local script.
//
// Resolution order, most explicit first:
//
//  1. --socket, if given
//  2. DOCKER_HOST, the variable every Docker tool already honours
//  3. the endpoint of the active `docker context`, which is where Colima, Rancher and
//     Docker Desktop each record their own answer
//  4. the conventional locations for this OS
//
// Step 3 is the one that makes this work by itself on a machine nobody described to it: the
// user has already told Docker where their daemon is, so ask Docker rather than guess.

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// dockerEndpoint is a resolved daemon address: a network Go can dial, and an address.
type dockerEndpoint struct {
	Network string // "unix" or "tcp"
	Address string // socket path, or host:port
	Source  string // how it was found, for error messages that explain themselves
}

func (e dockerEndpoint) String() string { return e.Network + "://" + e.Address }

// parseDockerHost understands the forms DOCKER_HOST and docker contexts use.
func parseDockerHost(raw string) (dockerEndpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return dockerEndpoint{}, fmt.Errorf("empty docker host")
	}

	// A bare path is a socket. Accepting it keeps `--socket /var/run/docker.sock` working
	// for anyone who reaches for the obvious thing.
	if !strings.Contains(raw, "://") {
		return dockerEndpoint{Network: "unix", Address: raw}, nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return dockerEndpoint{}, fmt.Errorf("bad docker host %q: %w", raw, err)
	}

	switch u.Scheme {
	case "unix":
		return dockerEndpoint{Network: "unix", Address: u.Path}, nil
	case "tcp", "http":
		return dockerEndpoint{Network: "tcp", Address: u.Host}, nil
	case "https":
		// Refused rather than silently downgraded: a TLS daemon needs client certs this
		// does not carry, and connecting without them would fail later and less clearly.
		return dockerEndpoint{}, fmt.Errorf("docker host %q uses TLS, which sbx does not configure; "+
			"use a local socket or an untrusted-network-free tcp:// endpoint", raw)
	case "npipe":
		// Named pipes need a Windows-only dialer that is not in the standard library, and
		// this binary's whole argument is that it has no dependencies. On Windows, run
		// under WSL2 - which is where Docker Desktop exposes a unix socket anyway.
		return dockerEndpoint{}, fmt.Errorf("docker host %q uses a Windows named pipe, which sbx cannot dial; "+
			"run sbx inside WSL2, or set DOCKER_HOST to a tcp:// endpoint", raw)
	default:
		return dockerEndpoint{}, fmt.Errorf("unsupported docker host scheme %q in %q", u.Scheme, raw)
	}
}

// defaultSocketPaths are the conventional locations, in the order worth trying.
func defaultSocketPaths() []string {
	home, _ := os.UserHomeDir()

	paths := []string{"/var/run/docker.sock"}

	if home == "" {
		return paths
	}

	return append(paths,
		filepath.Join(home, ".colima", "default", "docker.sock"),
		filepath.Join(home, ".rd", "docker.sock"),                // Rancher Desktop
		filepath.Join(home, ".docker", "run", "docker.sock"),     // Docker Desktop
		filepath.Join(home, ".docker", "desktop", "docker.sock"), // Docker Desktop, older
		filepath.Join(home, ".local", "share", "containers", "podman", // rootless podman
			"machine", "podman.sock"),
	)
}

// contextEndpoint asks the docker CLI where the active context points.
func contextEndpoint() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "context", "inspect",
		"--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		return "", false
	}

	host := strings.TrimSpace(string(out))

	return host, host != ""
}

// resolveDockerHost applies the order above and returns the first endpoint that exists.
func resolveDockerHost(flagValue string) (dockerEndpoint, error) {
	if flagValue != "" {
		ep, err := parseDockerHost(flagValue)
		if err != nil {
			return ep, err
		}

		ep.Source = "--socket"

		return ep, nil
	}

	if env := os.Getenv("DOCKER_HOST"); env != "" {
		ep, err := parseDockerHost(env)
		if err != nil {
			return ep, err
		}

		ep.Source = "DOCKER_HOST"

		return ep, nil
	}

	if host, ok := contextEndpoint(); ok {
		if ep, err := parseDockerHost(host); err == nil {
			ep.Source = "docker context"

			if ep.Network == "tcp" || exists(ep.Address) {
				return ep, nil
			}
		}
	}

	for _, p := range defaultSocketPaths() {
		if exists(p) {
			return dockerEndpoint{Network: "unix", Address: p, Source: "default location"}, nil
		}
	}

	return dockerEndpoint{}, fmt.Errorf(
		"no docker daemon found on %s: set DOCKER_HOST, or pass --socket. Looked at the active "+
			"docker context and %v", runtime.GOOS, defaultSocketPaths())
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// dial reaches the daemon, whichever kind of endpoint it turned out to be.
func (e dockerEndpoint) dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, e.Network, e.Address)
}

func SortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// PortPair is one docker "wake:backing" pair from a container label.
type PortPair struct {
	Public  int
	Backing int
}

// parsePorts reads "20002:30002,20003:30003".
func ParsePorts(label string) ([]PortPair, error) {
	if strings.TrimSpace(label) == "" {
		return nil, fmt.Errorf("missing ports label")
	}

	var out []PortPair

	for _, pair := range strings.Split(label, ",") {
		pub, back, ok := strings.Cut(strings.TrimSpace(pair), ":")
		if !ok {
			return nil, fmt.Errorf("bad port pair %q", pair)
		}

		p, err := strconv.Atoi(pub)
		if err != nil {
			return nil, fmt.Errorf("bad public port %q: %w", pub, err)
		}

		b, err := strconv.Atoi(back)
		if err != nil {
			return nil, fmt.Errorf("bad backing port %q: %w", back, err)
		}

		out = append(out, PortPair{Public: p, Backing: b})
	}

	return out, nil
}
