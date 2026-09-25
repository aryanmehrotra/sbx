package fchost

// The Mac (or Windows) half of `sbx serve --provider firecracker`.
//
// It serves nothing itself. It makes sure the helper VM is up with the right sbx in it and that
// sbx's daemon running, then does two things with the daemon's two loopback listeners, both
// reached through one ssh forward:
//
//   - connect (22980 in the VM) is followed by daemon.Mirror, which binds every sandbox port on
//     this machine's loopback at the SAME number the in-VM `sbx env` prints, and carries each
//     connection over the existing WebSocket tunnel. A TCP connect on the Mac reaches the in-VM
//     daemon's listener, which is what wakes the microVM. Connect-to-wake survives the hop.
//   - OSB (22981 in the VM) can be reverse-proxied at --osb-addr, but is not today: an OpenSandbox
//     API sandbox needs sbx's agent mounted into it as a volume, which a microVM cannot take yet,
//     so ServeMain refuses --osb-addr (provider.ErrOSBOnFirecracker) and the in-VM daemon is
//     started without an OSB listener. The proxy below is kept for when it can.
//
// The rest of the CLI (create, list, env, exec, logs, rm, ...) is redirected into the VM by
// Redirect, so it needs none of this - which is why this file is small.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/osb"
)

// EnsureOptions is what the VM needs for its daemon to be the right one.
type EnsureOptions struct {
	Version string
	OSBKey  string
	Serve   []string

	// OSB starts the in-VM OpenSandbox listener; see DaemonOptions.OSB.
	OSB bool

	// Binary finds the linux sbx to install. Defaults to osb.AgentFile for the host's own
	// architecture: vz and WSL2 both run guests of the host's architecture only.
	Binary func(ctx context.Context) (string, error)
}

// Ensure brings the VM to "running, with this sbx, with its daemon up". Idempotent, and cheap
// when it already is: one listing, one hash, one systemctl.
func (m *Manager) Ensure(ctx context.Context, opt EnsureOptions) error {
	if err := m.Start(ctx); err != nil {
		return err
	}

	if err := m.Provision(ctx); err != nil {
		return err
	}

	find := opt.Binary
	if find == nil {
		find = func(ctx context.Context) (string, error) {
			return osb.AgentFile(ctx, opt.Version, runtime.GOARCH)
		}
	}

	changed := false

	file, err := find(ctx)

	switch {
	case errors.Is(err, osb.ErrPublishedOnly):
		have, _ := m.inVM(ctx, nil, guestBinary, "version")
		if !strings.Contains(have, strings.TrimPrefix(opt.Version, "v")) {
			if err := m.InstallRelease(ctx, opt.Version); err != nil {
				return err
			}

			changed = true
		}
	case err != nil:
		return err
	default:
		if changed, err = m.Install(ctx, file); err != nil {
			return err
		}
	}

	tok, err := m.Token()
	if err != nil {
		return err
	}

	return m.StartDaemon(ctx, DaemonOptions{Token: tok, OSBKey: opt.OSBKey, Serve: opt.Serve, Restart: changed, OSB: opt.OSB})
}

// FrontOptions is the host-side `sbx serve --provider firecracker`.
type FrontOptions struct {
	EnsureOptions

	// OSBAddr is where the OpenSandbox API answers on this machine; empty for none.
	OSBAddr string

	// Refresh is how often the mirror asks what the VM is fronting.
	Refresh time.Duration

	// Shift moves every local port, as `sbx connect --port-offset` does. Zero in production;
	// a test needs it because both ends share one loopback.
	Shift int

	// tunnel replaces the ssh forward, for tests.
	tunnel func(ctx context.Context) (Endpoints, func() error, error)

	// skipEnsure is for tests whose "VM" is an httptest server.
	skipEnsure bool
}

// Front runs until ctx ends or the tunnel to the VM dies.
func (m *Manager) Front(ctx context.Context, opt FrontOptions) error {
	if opt.OSBAddr != "" && opt.OSBKey == "" && !loopback(opt.OSBAddr) {
		return fmt.Errorf("--osb-addr %s is not loopback, so it needs --osb-key (or SBX_OSB_KEY): "+
			"the API it proxies creates and runs code", opt.OSBAddr)
	}

	opt.EnsureOptions.OSB = opt.OSBAddr != ""

	if !opt.skipEnsure {
		if err := m.Ensure(ctx, opt.EnsureOptions); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tunnel := opt.tunnel
	if tunnel == nil {
		tunnel = m.Tunnel
	}

	eps, wait, err := tunnel(ctx)
	if err != nil {
		return err
	}

	if err := waitHealthy(ctx, eps.Connect+"/healthz", 60*time.Second); err != nil {
		return fmt.Errorf("the daemon in the helper VM never answered: %w - read its log with: %s",
			err, strings.Join(m.shellHint("journalctl", "-u", guestUnit, "-n", "50"), " "))
	}

	tok, err := m.Token()
	if err != nil {
		return err
	}

	m.say("helper VM %s (%s) is serving; sandbox ports appear here as they are created", m.Config.Name, m.Driver.Name())

	errc := make(chan error, 3)

	go func() {
		if err := wait(); err != nil && ctx.Err() == nil {
			errc <- fmt.Errorf("the tunnel to the helper VM closed: %w", err)

			return
		}

		if ctx.Err() == nil {
			errc <- errors.New("the tunnel to the helper VM closed")
		}
	}()

	// WSL2 already forwards every guest loopback port to the host at the same number; binding
	// them a second time would only collide with its own relay.
	if !m.Driver.NativeForwarding() {
		go func() {
			errc <- daemon.Mirror(ctx, daemon.MirrorOptions{
				Endpoint: daemon.Endpoint{Label: m.Config.Name, URL: eps.Connect, Token: tok},
				Refresh:  opt.Refresh, Shift: opt.Shift, Out: m.Out,
			})
		}()
	}

	if opt.OSBAddr != "" {
		srv, err := osbProxy(opt.OSBAddr, eps.OSB)
		if err != nil {
			return err
		}

		m.say("OpenSandbox API on http://%s (proxied into %s)", srv.Addr, m.Config.Name)

		go func() { errc <- serveUntil(ctx, srv) }()
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		if ctx.Err() != nil {
			return nil
		}

		return err
	}
}

func (m *Manager) shellHint(argv ...string) []string {
	switch m.Driver.Name() {
	case "lima":
		return append([]string{"limactl", "shell", m.Config.Name, "sudo"}, argv...)
	case "colima":
		return append([]string{"colima", "ssh", "--profile", m.Config.Name, "--", "sudo"}, argv...)
	default:
		return append([]string{"wsl.exe", "-d", m.Config.Name, "-u", "root", "--"}, argv...)
	}
}

func waitHealthy(ctx context.Context, u string, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}

	var last error

	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)

		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				return nil
			}

			err = fmt.Errorf("%s answered %s", u, resp.Status)
		}

		last = err

		select {
		case <-ctx.Done():
			return last
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// osbProxy forwards the API byte for byte. The key, if any, is checked by the daemon in the VM
// - it was started with the same one - so this layer cannot disagree with it.
func osbProxy(addr, target string) (*http.Server, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	rp := httputil.NewSingleHostReverseProxy(u)

	// Streaming endpoints (command output, SSE) must not sit in a buffer.
	rp.FlushInterval = -1

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("--osb-addr %s: %w", addr, err)
	}

	srv := &http.Server{Addr: ln.Addr().String(), Handler: rp, ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = srv.Serve(ln) }()

	return srv, nil
}

func serveUntil(ctx context.Context, srv *http.Server) error {
	<-ctx.Done()

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = srv.Shutdown(sctx)

	return nil
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
