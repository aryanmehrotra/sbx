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
//   - OSB (22981 in the VM), started only for --osb-addr, is reverse-proxied there. The API runs
//     in the VM exactly as on a Linux host with /dev/kvm - its daemon, the jailer, the egress
//     filter and the guard are all the VM's, native - so this side adds only transport and two
//     checks: it will not front the API without a key, and before serving it proves the API
//     behind the tunnel refuses a request without that key and accepts it with it (the ssh
//     forward is a port on this machine's loopback, which containers on a VM-backed engine
//     reach). A create, or an endpoint lookup, is answered only after the mirror has bound the
//     sandbox's ports here, so an SDK that dials the endpoint at once finds it listening.
//
// The rest of the CLI (create, list, env, exec, logs, rm, ...) is redirected into the VM by
// Redirect, so it needs none of this - which is why this file is small.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

	// SetDaemon says OSBKey, Serve and OSB are what the in-VM daemon is to run with - `sbx serve`
	// says so - and records them. Every other Ensure (a redirected command, Remote, `sbx fc vm
	// start`) has no opinion and uses the recorded ones: a redirected `sbx create` must not restart
	// the daemon without the --osb-key or --idle a running `sbx serve` gave it.
	SetDaemon bool

	// Binary finds the linux sbx to install. Defaults to osb.AgentFile for the host's own
	// architecture: vz and WSL2 both run guests of the host's architecture only.
	Binary func(ctx context.Context) (string, error)
}

// Ensure brings the VM to "running, with this sbx, with its daemon up". Idempotent, and cheap
// when it already is: one listing, one hash, one systemctl.
func (m *Manager) Ensure(ctx context.Context, opt EnsureOptions) error {
	unlock, err := m.lockVM(ctx)
	if err != nil {
		return err
	}
	defer unlock()

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

	d := daemonConfig{OSBKey: opt.OSBKey, Serve: opt.Serve, OSB: opt.OSB}

	if opt.SetDaemon {
		if err := m.saveDaemonConfig(d); err != nil {
			return err
		}
	} else if saved, ok := m.loadDaemonConfig(); ok {
		d = saved
	}

	if err := m.StartDaemon(ctx, DaemonOptions{Token: tok, OSBKey: d.OSBKey, Serve: d.Serve, Restart: changed, OSB: d.OSB}); err != nil {
		return err
	}

	m.writeStamp(opt.Version)

	return nil
}

// daemonConfig is what `sbx serve` last asked the in-VM daemon to run with, kept host-side in the
// state directory (0600: it can hold the OSB key) so an Ensure that has no opinion keeps it.
type daemonConfig struct {
	OSBKey string   `json:"osb_key,omitempty"`
	Serve  []string `json:"serve,omitempty"`
	OSB    bool     `json:"osb,omitempty"`
}

func (m *Manager) daemonConfigPath() string {
	return filepath.Join(m.StateDir, "daemon-"+m.Config.Name+".json")
}

func (m *Manager) saveDaemonConfig(d daemonConfig) error {
	if err := os.MkdirAll(m.StateDir, 0o700); err != nil {
		return err
	}

	b, err := json.Marshal(d)
	if err != nil {
		return err
	}

	return os.WriteFile(m.daemonConfigPath(), b, 0o600)
}

func (m *Manager) loadDaemonConfig() (daemonConfig, bool) {
	b, err := os.ReadFile(m.daemonConfigPath())
	if err != nil {
		return daemonConfig{}, false
	}

	var d daemonConfig

	return d, json.Unmarshal(b, &d) == nil
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

	// keyNote says where the API key came from, for the startup line; never the key.
	keyNote string
}

// Front runs until ctx ends or the tunnel to the VM dies.
func (m *Manager) Front(ctx context.Context, opt FrontOptions) error {
	// The same bind rule as a docker-backed `sbx serve --osb-addr`: loopback only, key or not. The
	// sandbox endpoints the API hands out are 127.0.0.1 listeners here, so an API served off this
	// machine would hand out addresses its callers cannot reach - and would be reachable itself.
	if opt.OSBAddr != "" {
		if err := osb.CheckBind(opt.OSBAddr, opt.OSBKey); err != nil {
			return err
		}

		// ServeMain always resolves one (flag, env, or ~/.sbx/osb/key). Fail closed anyway: a
		// keyless in-VM API would be open to every container that reaches this loopback.
		if opt.OSBKey == "" {
			return errors.New("the OpenSandbox API through the helper VM is served with a key or not at all: " +
				"pass --osb-key, set SBX_OSB_KEY, or let sbx serve generate one into ~/.sbx/osb/key")
		}
	}

	opt.EnsureOptions.OSB = opt.OSBAddr != ""
	opt.EnsureOptions.SetDaemon = true

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

	if opt.OSBAddr != "" {
		if err := checkKeyed(ctx, eps.OSB, opt.OSBKey, 60*time.Second); err != nil {
			return err
		}
	}

	tok, err := m.Token()
	if err != nil {
		return err
	}

	m.say("helper VM %s (%s) is serving; sandbox ports appear here as they are created", m.Config.Name, m.Driver.Name())

	errc := make(chan error, 3)

	// Front returns only once what it bound is closed: the mirror's local ports and the API
	// listener. Returning on ctx while they were still being torn down left a mirrored port
	// accepting after Front said it was done - a restart of `sbx serve` could then fail to bind it.
	var listeners sync.WaitGroup

	defer func() {
		cancel()
		listeners.Wait()
	}()

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
	var kick chan chan struct{}

	if !m.Driver.NativeForwarding() {
		kick = make(chan chan struct{})

		listeners.Add(1)

		go func() {
			defer listeners.Done()

			errc <- daemon.Mirror(ctx, daemon.MirrorOptions{
				Endpoint: daemon.Endpoint{Label: m.Config.Name, URL: eps.Connect, Token: tok},
				Refresh:  opt.Refresh, Shift: opt.Shift, Out: m.Out, Sync: kick,
			})
		}()
	}

	if opt.OSBAddr != "" {
		srv, err := osbProxy(opt.OSBAddr, eps.OSB, kick)
		if err != nil {
			return err
		}

		m.say("OpenSandbox API on http://%s (served in %s, OPEN-SANDBOX-API-KEY required: %s)",
			srv.Addr, m.Config.Name, orUnknown(opt.keyNote))

		listeners.Add(1)

		go func() {
			defer listeners.Done()

			errc <- serveUntil(ctx, srv)
		}()
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

// checkKeyed proves the API behind target is keyed with key before anything is fronted: a
// request without the key must be refused (401), and one with it accepted. The first is the
// control that matters - the ssh forward it arrives through is a port on this machine's loopback,
// reachable from containers on a VM-backed engine whatever this process checks - and the second
// catches an in-VM daemon still holding another key, which would refuse every client here.
func checkKeyed(ctx context.Context, target, key string, limit time.Duration) error {
	if err := waitHealthy(ctx, target+"/health", limit); err != nil {
		return fmt.Errorf("the OpenSandbox API in the helper VM never answered: %w", err)
	}

	status := func(withKey bool) (int, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/v1/sandboxes", nil)
		if err != nil {
			return 0, err
		}

		if withKey {
			req.Header.Set("OPEN-SANDBOX-API-KEY", key)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		return resp.StatusCode, nil
	}

	code, err := status(false)
	if err != nil {
		return fmt.Errorf("asking the helper VM's OpenSandbox API without the key: %w", err)
	}

	if code != http.StatusUnauthorized {
		return fmt.Errorf("the OpenSandbox API in the helper VM answered %d to a request without the key, "+
			"where it must refuse with 401: refusing to front it, because its forward on this machine's "+
			"loopback is reachable from containers. Restart `sbx serve --provider firecracker --osb-addr ...`", code)
	}

	code, err = status(true)
	if err != nil {
		return fmt.Errorf("asking the helper VM's OpenSandbox API with the key: %w", err)
	}

	if code != http.StatusOK {
		return fmt.Errorf("the OpenSandbox API in the helper VM rejects the key this machine holds (HTTP %d): "+
			"its daemon is running with another one. Restart `sbx serve --provider firecracker --osb-addr ...`", code)
	}

	return nil
}

// osbProxy forwards the API byte for byte. The key is checked by the daemon in the VM - it was
// started with the same one, which checkKeyed proved - so this layer cannot disagree with it.
//
// kick, when set, is the mirror's Sync: a create (any POST under /v1/sandboxes) or an endpoint
// lookup that succeeded is held until the mirror has bound what the VM now fronts, so the
// endpoint in the answer is listening here when the caller dials it. Bounded, so a mirror that
// is down delays an answer by seconds, never forever.
func osbProxy(addr, target string, kick chan<- chan struct{}) (*http.Server, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	rp := httputil.NewSingleHostReverseProxy(u)

	// Streaming endpoints (command output, SSE) must not sit in a buffer.
	rp.FlushInterval = -1

	if kick != nil {
		rp.ModifyResponse = func(resp *http.Response) error {
			if mirrorsBefore(resp.Request, resp.StatusCode) {
				syncMirror(resp.Request.Context(), kick, 5*time.Second)
			}

			return nil
		}
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("--osb-addr %s: %w", addr, err)
	}

	srv := &http.Server{Addr: ln.Addr().String(), Handler: rp, ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = srv.Serve(ln) }()

	return srv, nil
}

// mirrorsBefore says whether an API answer can carry an endpoint the mirror may not have bound
// yet: a successful create, resume or renew (POST), or an endpoint lookup.
func mirrorsBefore(r *http.Request, code int) bool {
	if code < 200 || code > 299 || !strings.HasPrefix(r.URL.Path, "/v1/sandboxes") {
		return false
	}

	return r.Method == http.MethodPost || (r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/"))
}

// syncMirror asks the mirror to reconcile now and waits for it, at most limit.
func syncMirror(ctx context.Context, kick chan<- chan struct{}, limit time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	done := make(chan struct{})

	select {
	case kick <- done:
	case <-ctx.Done():
		return
	}

	select {
	case <-done:
	case <-ctx.Done():
	}
}

func serveUntil(ctx context.Context, srv *http.Server) error {
	<-ctx.Done()

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = srv.Shutdown(sctx)

	return nil
}
