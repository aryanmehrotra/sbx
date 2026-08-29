package tunnel

// A public link to a sleeping sandbox.
//
//	sbx url feature-x app
//
// The point is not the tunnel - it is what the tunnel points at. It is aimed at the wake
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
	// args builds the command line for a local port. rewriteHost asks the backend to send the
	// origin `Host: 127.0.0.1:<port>` instead of the public tunnel hostname; a backend that
	// cannot is passed it anyway and ignores it, and says so through hostRewrite.
	args func(port int, rewriteHost bool) []string

	// hostRewrite reports whether this backend can rewrite the Host header at all. Measured,
	// not assumed: cloudflared has --http-host-header, and ngrok removed its --host-header
	// flag in v3 in favour of a traffic-policy file. A backend that cannot do it says so
	// rather than accepting the request and quietly not honouring it.
	hostRewrite bool
	// url finds the public address in the tool's own output.
	url *regexp.Regexp
	// notURL are hostnames the pattern matches that are never the tunnel — a
	// backend's own control plane, typically.
	notURL map[string]bool
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
			name:        "cloudflared",
			bin:         "cloudflared",
			hostRewrite: true,
			args: func(p int, rewriteHost bool) []string {
				a := []string{
					"tunnel", "--no-autoupdate",

					// cloudflared's own metrics listener otherwise takes the first free port
					// of 20241-20245, and sbx hands out 20000-21199 (publicBase + slot*20,
					// 60 slots) - so 20241-20245 is slot 12. Measured: with nothing on those
					// ports, cloudflared binds 20241 and the daemon can no longer listen on
					// it. `sbx url` would be taking a port out from under the daemon it
					// depends on, and only when slot 12 happened to be unallocated - an
					// intermittent bind failure with no visible cause. :0 is an ephemeral
					// port, which is outside the range by construction.
					"--metrics", "127.0.0.1:0",

					"--url", "http://localhost:" + strconv.Itoa(p),
				}

				if rewriteHost {
					// Without this the origin sees `Host: <name>.trycloudflare.com`, and
					// every dev server that checks the header refuses it - measured against
					// vite, which answers 403 "Blocked request. This host is not allowed."
					// Same class in webpack-dev-server, Rails and Django. Sharing a branch
					// preview is what `sbx url` is for, so the header it sends should be the
					// one the thing being previewed will accept.
					a = append(a, "--http-host-header", "127.0.0.1:"+strconv.Itoa(p))
				}

				return a
			},
			// api.trycloudflare.com is cloudflared's OWN control plane, and it
			// logs a call to it BEFORE printing the tunnel hostname. Matching the
			// first hit therefore handed out a URL that answers 405 from
			// Cloudflare's API and never reaches the sandbox at all. Excluded by
			// name rather than by shape: quick-tunnel hostnames are ordinary
			// words, so nothing about the form distinguishes them.
			url:    regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`),
			notURL: map[string]bool{"https://api.trycloudflare.com": true},
		},
		{
			name: "ngrok",
			bin:  "ngrok",
			// ngrok v3 removed --host-header; the equivalent is a traffic-policy file, which
			// is a file to write and clean up rather than a flag. Left unsupported and
			// declared, so `sbx url --host-header rewrite --via ngrok` says it cannot rather
			// than accepting the flag and passing the public hostname through anyway.
			hostRewrite: false,
			args: func(p int, _ bool) []string {
				return []string{"http", strconv.Itoa(p), "--log", "stdout", "--log-format", "logfmt"}
			},
			url:  regexp.MustCompile(`url=(https://[^\s]+\.ngrok[^\s]*)`),
			note: "ngrok needs an authtoken configured (ngrok config add-authtoken ...)",
		},
		{
			// Opt-in only, via --via ssh.
			//
			// It routes your traffic through localhost.run, a third party with no account,
			// no access control, and - checked, not assumed - no published host key
			// fingerprint to pin against. All three are fine to choose deliberately and
			// wrong to arrive at because something else failed.
			name:     "ssh",
			bin:      "ssh",
			explicit: true,
			// -R forwards a TCP port. There is no HTTP layer here to rewrite a header in;
			// localhost.run's own front end sets the Host, and nothing on this side can.
			hostRewrite: false,
			args: func(p int, _ bool) []string {
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
				"        host key - so this trusts the entry already in your known_hosts",
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
		"no tunnel available. Install one - cloudflared needs no account:\n" +
			"  brew install cloudflared   |   https://github.com/cloudflare/cloudflared\n" +
			"Or pass --via ssh to route through localhost.run, a third party with no access control.")
}

// Open points a tunnel at a local port and blocks, printing the link.
//
// A port, not a sandbox. This package has no idea what a sandbox is, which is why it can be
// read on its own and why nothing here has to change when the thing behind the port does.
func Open(ctx context.Context, label string, port int, preferred string, rewriteHost bool) error {
	backends, err := pickTunnels(preferred)
	if err != nil {
		return err
	}

	// Asking a named backend for something it cannot do is an error, not a downgrade. The
	// alternative - accept the flag and send the public hostname anyway - is the failure shape
	// this project keeps finding: reported success, reached nothing.
	if rewriteHost && preferred != "" && !backends[0].hostRewrite {
		return fmt.Errorf(
			"%s cannot rewrite the Host header (only cloudflared can); "+
				"pass --host-header pass to send the tunnel's own hostname to the service",
			preferred)
	}

	for i, b := range backends {
		err := runTunnel(ctx, b, label, port, rewriteHost)
		if err == nil || ctx.Err() != nil {
			return nil
		}

		if i < len(backends)-1 {
			// Only ever between backends the caller already consented to; an explicit one is
			// not in this list unless it was named.
			fmt.Printf("  %s did not come up (%v) - trying %s\n", b.name, err, backends[i+1].name)
		} else {
			return fmt.Errorf("no tunnel came up; last was %s: %w", b.name, err)
		}
	}

	return nil
}

func runTunnel(ctx context.Context, b tunnelBackend, label string, port int, rewriteHost bool) error {
	fmt.Printf("sbx url · %s · via %s · wake port %d\n", label, b.name, port)

	if b.note != "" {
		fmt.Printf("  note: %s\n", b.note)
	}

	// Said out loud either way, because it is the one thing about this link that can change
	// what the service on the other end does with a request.
	switch {
	case rewriteHost && b.hostRewrite:
		fmt.Printf("  the service is sent Host: 127.0.0.1:%d, so a dev server that checks the\n"+
			"        header serves it. --host-header pass sends the public hostname instead\n", port)
	case rewriteHost:
		fmt.Printf("  %s cannot rewrite Host, so the service is sent the public hostname -\n"+
			"        a dev server that checks the header (vite, Rails, Django) will refuse it\n", b.name)
	}

	cmd := exec.CommandContext(ctx, b.bin, b.args(port, rewriteHost && b.hostRewrite)...)

	// Tunnels announce themselves on whichever stream they feel like; merge and scan both.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	// Merged into stdout, because these binaries disagree about which stream the URL goes to
	// and scanning one of them is the whole job.
	//
	// Nothing else is needed here. StderrPipe() used to be called straight after this and its
	// error ignored - but Cmd.StderrPipe returns an error whenever Cmd.Stderr is already set,
	// which it now is, so that goroutine never ran. Dead code that reads as working is worse
	// than none: the next person to rely on it gets silence.
	cmd.Stderr = cmd.Stdout.(io.Writer)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", b.bin, err)
	}

	found := false
	scanner := bufio.NewScanner(stdout)

	// A tunnel binary announcing something long - a banner, a JSON blob - would otherwise
	// exceed bufio's 64 KB default, end the scan silently, and leave the child blocked
	// writing into a pipe nobody reads. `sbx url` would hang rather than fail.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		if !found {
			if m := b.url.FindStringSubmatch(line); m != nil && !b.notURL[m[len(m)-1]] {
				link := m[len(m)-1]
				found = true

				fmt.Printf("\n  %s\n\n", link)
				fmt.Println("  Opening it wakes the sandbox. Ctrl-C closes the tunnel; the sandbox stays.")
			}
		}
	}

	// Checked, so a scan that stopped for a reason other than EOF says so rather than looking
	// like a backend that simply never printed a URL.
	scanErr := scanner.Err()

	waitErr := cmd.Wait()

	if !found && scanErr != nil {
		return fmt.Errorf("reading %s output: %w", b.bin, scanErr)
	}

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
