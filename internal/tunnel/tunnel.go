package tunnel

// A public link to a sleeping sandbox.
//
//	sbx url feature-x app
//
// The point is not the tunnel — it is what the tunnel points at. It is aimed at the wake
// port, the one `sbx serve` owns, so the sandbox behind a shared link is asleep until
// somebody opens it and costs nothing the rest of the time. A reviewer clicks a URL, waits
// about a second, and sees the app.
//
// sbx does not implement a tunnel and should not. Cloudflare reached the same conclusion
// about their own sandbox SDK in 2026 and replaced `exposePort()` with Cloudflare Tunnel.
// This shells out to whichever tunnel the machine already has, in preference order, and
// tells you what to install if it has none.
//
// HTTP only, and deliberately: the free tunnels of every provider are HTTP. Publishing a
// database port to the internet is not a feature to add casually, and anyone who genuinely
// wants it wants a named tunnel with access control rather than a one-liner.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type tunnelBackend struct {
	name string
	bin  string
	// args builds the command line for a local port.
	args func(port int) []string
	// url finds the public address in the tool's own output.
	url *regexp.Regexp
	// note is shown once, when a backend needs the reader to know something.
	note string

	// explicit backends are never chosen automatically. A tool that silently falls back to
	// routing your traffic through a stranger is failing toward less trust, which is the
	// wrong direction for a default.
	explicit bool

	// hostKey pins the server, where the operator publishes one to pin against. Empty means
	// no authoritative fingerprint exists, and the backend leans on the user's own
	// known_hosts rather than accepting whatever answers.
	hostKey string
}

// backends, best first.
//
// cloudflared leads because its quick tunnels need no account at all. ssh is last and
// matters most: it is already on every machine, so `sbx url` works with nothing installed.
func tunnelBackends() []tunnelBackend {
	return []tunnelBackend{
		{
			name: "cloudflared",
			bin:  "cloudflared",
			args: func(p int) []string {
				return []string{"tunnel", "--no-autoupdate", "--url", "http://localhost:" + strconv.Itoa(p)}
			},
			url: regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`),
		},
		{
			name: "ngrok",
			bin:  "ngrok",
			args: func(p int) []string {
				return []string{"http", strconv.Itoa(p), "--log", "stdout", "--log-format", "logfmt"}
			},
			url:  regexp.MustCompile(`url=(https://[^\s]+\.ngrok[^\s]*)`),
			note: "ngrok needs an authtoken configured (ngrok config add-authtoken ...)",
		},
		{
			// Opt-in only, via --via ssh.
			//
			// It routes your traffic through localhost.run, a third party with no account,
			// no access control, and — checked, not assumed — no published host key
			// fingerprint to pin against. All three are fine to choose deliberately and
			// wrong to arrive at because something else failed.
			name:     "ssh",
			bin:      "ssh",
			explicit: true,
			args: func(p int) []string {
				return []string{
					// Not accept-new. Trusting whatever answers on first contact is the
					// thing pinning exists to prevent, and doing it inside a fallback means
					// nobody was watching when it happened.
					"-o", "StrictHostKeyChecking=yes",
					"-o", "ServerAliveInterval=30",
					"-R", "80:localhost:" + strconv.Itoa(p),
					"nokey@localhost.run",
				}
			},
			url: regexp.MustCompile(`https://[a-z0-9-]+\.lhr\.life`),
			note: "localhost.run: third party, no account, no access control, and no published\n" +
				"        host key — so this trusts the entry already in your known_hosts",
		},
	}
}

// pickTunnels returns every usable backend, best first.
//
// Plural because installed is not the same as working: ngrok is on plenty of machines
// without an authtoken, and it exits immediately when asked to serve. The caller tries them
// in order rather than failing on the first one that happens to be present.
func pickTunnels(preferred string) ([]tunnelBackend, error) {
	var (
		usable    []tunnelBackend
		installed []string
	)

	for _, b := range tunnelBackends() {
		if _, err := exec.LookPath(b.bin); err != nil {
			continue
		}

		installed = append(installed, b.name)

		// An explicit backend has to be asked for by name. Being installed is not consent.
		if b.explicit && preferred != b.name {
			continue
		}

		if preferred == "" || preferred == b.name {
			usable = append(usable, b)
		}
	}

	if len(usable) > 0 {
		return usable, nil
	}

	if preferred != "" {
		return nil, fmt.Errorf("tunnel %q is not installed (available here: %s)",
			preferred, strings.Join(installed, ", "))
	}

	return nil, fmt.Errorf(
		"no tunnel available. Install one — cloudflared needs no account:\n" +
			"  brew install cloudflared   |   https://github.com/cloudflare/cloudflared\n" +
			"Or pass --via ssh to route through localhost.run, a third party with no access control.")
}

// Open points a tunnel at a local port and blocks, printing the link.
//
// A port, not a sandbox. This package has no idea what a sandbox is, which is why it can be
// read on its own and why nothing here has to change when the thing behind the port does.
func Open(ctx context.Context, label string, port int, preferred string) error {
	backends, err := pickTunnels(preferred)
	if err != nil {
		return err
	}

	for i, b := range backends {
		err := runTunnel(ctx, b, label, port)
		if err == nil || ctx.Err() != nil {
			return nil
		}

		if i < len(backends)-1 {
			// Only ever between backends the caller already consented to; an explicit one is
			// not in this list unless it was named.
			fmt.Printf("  %s did not come up (%v) — trying %s\n", b.name, err, backends[i+1].name)
		} else {
			return fmt.Errorf("no tunnel came up; last was %s: %w", b.name, err)
		}
	}

	return nil
}

func runTunnel(ctx context.Context, b tunnelBackend, label string, port int) error {
	fmt.Printf("sbx url · %s · via %s · wake port %d\n", label, b.name, port)

	if b.note != "" {
		fmt.Printf("  note: %s\n", b.note)
	}

	cmd := exec.CommandContext(ctx, b.bin, b.args(port)...)

	// Tunnels announce themselves on whichever stream they feel like; merge and scan both.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	cmd.Stderr = cmd.Stdout.(io.Writer)

	stderr, err := cmd.StderrPipe()
	if err == nil {
		go func() { _, _ = io.Copy(io.Discard, stderr) }()
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", b.bin, err)
	}

	found := false
	scanner := bufio.NewScanner(stdout)

	for scanner.Scan() {
		line := scanner.Text()

		if !found {
			if m := b.url.FindStringSubmatch(line); m != nil {
				link := m[len(m)-1]
				found = true

				fmt.Printf("\n  %s\n\n", link)
				fmt.Println("  Opening it wakes the sandbox. Ctrl-C closes the tunnel; the sandbox stays.")
			}
		}
	}

	waitErr := cmd.Wait()

	// Announcing a URL is the definition of working. A backend that exits without one has
	// not tunnelled anything, whatever its exit code said.
	if !found {
		if waitErr == nil {
			waitErr = fmt.Errorf("exited without announcing a URL")
		}

		return waitErr
	}

	if waitErr != nil && ctx.Err() == nil {
		return fmt.Errorf("%s exited: %w", b.bin, waitErr)
	}

	return nil
}
