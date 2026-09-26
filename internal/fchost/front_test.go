package fchost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const runningLima = `{"name":"sbx-fc","status":"Running","sshConfigFile":"/lima/sbx-fc/ssh.config"}`

func limaManager(t *testing.T, r *fakeRunner) *Manager {
	t.Helper()

	return current(t, &Manager{Driver: lima{}, Config: testCfg, Run: r, Out: io.Discard, StateDir: t.TempDir()})
}

// current makes m's helper VM one this build already ensured, as it is on every command after the
// first: a test about something else is not also a test of the upgrade path.
func current(t *testing.T, m *Manager) *Manager {
	t.Helper()

	m.hostStamp = func(string) string { return "this-build" }
	m.writeStamp("")

	return m
}

// fakeVMDaemon is the in-VM `sbx serve --provider firecracker` as the host sees it through the
// tunnel: /healthz, and a /v1/fleet that fronts one port and demands the shared token.
func fakeVMDaemon(t *testing.T, token string, port int) *httptest.Server {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/v1/fleet":
			if r.Header.Get("Authorization") != "Bearer "+token {
				w.WriteHeader(http.StatusUnauthorized)

				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"services": []map[string]any{{
					"sandbox": "demo", "service": "db", "ref": "demo-db", "instance": "i1",
					"awake": false, "ports": []int{port},
				}},
				"halfClose": true,
			})
		default:
			http.NotFound(w, r)
		}
	}))

	t.Cleanup(ts.Close)

	return ts
}

func freeLocal(t *testing.T) int {
	t.Helper()

	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func up(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err == nil {
		_ = c.Close()
	}

	return err == nil
}

// The forwarding wiring end to end on the host side: the VM's fleet becomes local ports at the
// VM's numbers (shifted here, because both ends share one loopback), and the OSB API answers on
// --osb-addr with the request passed through untouched - key header included.
func TestFrontMirrorsTheVMsPortsAndProxiesItsAPI(t *testing.T) {
	m := limaManager(t, &fakeRunner{})

	tok, err := m.Token()
	if err != nil {
		t.Fatal(err)
	}

	remote := 45432
	vm := fakeVMDaemon(t, tok, remote)

	var gotKey, gotPath string

	api := keyedAPI(t, "k", func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("OPEN-SANDBOX-API-KEY"), r.URL.Path
		_, _ = io.WriteString(w, `{"items":[]}`)
	})

	local := freeLocal(t)
	osbAddr := fmt.Sprintf("127.0.0.1:%d", freeLocal(t))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- m.Front(ctx, FrontOptions{
			EnsureOptions: EnsureOptions{OSBKey: "k"},
			OSBAddr:       osbAddr, Refresh: 50 * time.Millisecond, Shift: local - remote, skipEnsure: true,
			tunnel: func(ctx context.Context) (Endpoints, func() error, error) {
				return Endpoints{Connect: vm.URL, OSB: api.URL}, func() error { <-ctx.Done(); return nil }, nil
			},
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !up(local) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if !up(local) {
		t.Fatalf("the VM's port %d was never mirrored at %d", remote, local)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://"+osbAddr+"/v1/sandboxes", nil)
	req.Header.Set("OPEN-SANDBOX-API-KEY", "k")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != 200 || string(body) != `{"items":[]}` || gotKey != "k" || gotPath != "/v1/sandboxes" {
		t.Fatalf("proxy: %d %q key=%q path=%q", resp.StatusCode, body, gotKey, gotPath)
	}

	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Front after cancel: %v", err)
	}

	if up(local) {
		t.Fatal("a mirrored port outlived the front")
	}
}

func TestFrontEndsWhenTheTunnelDies(t *testing.T) {
	m := limaManager(t, &fakeRunner{})

	tok, _ := m.Token()
	vm := fakeVMDaemon(t, tok, 45433)

	err := m.Front(context.Background(), FrontOptions{
		Refresh: 50 * time.Millisecond, Shift: freeLocal(t) - 45433, skipEnsure: true,
		tunnel: func(context.Context) (Endpoints, func() error, error) {
			return Endpoints{Connect: vm.URL}, func() error { return errors.New("exit status 255") }, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "tunnel to the helper VM closed") {
		t.Fatalf("a dead tunnel must end the front, loudly: %v", err)
	}
}

// Front binds the API by the docker path's rule: loopback only, and a key does not change that.
func TestFrontRefusesAnAPIOffLoopbackKeyOrNot(t *testing.T) {
	m := limaManager(t, &fakeRunner{})

	for _, key := range []string{"", "k"} {
		err := m.Front(context.Background(), FrontOptions{OSBAddr: "0.0.0.0:8080", skipEnsure: true,
			EnsureOptions: EnsureOptions{OSBKey: key}})
		if err == nil || !strings.Contains(err.Error(), "not a loopback address") {
			t.Fatalf("key %q: got %v", key, err)
		}
	}
}

func TestFrontNamesTheLogWhenTheDaemonNeverAnswers(t *testing.T) {
	m := limaManager(t, &fakeRunner{})

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	err := m.Front(ctx, FrontOptions{skipEnsure: true,
		tunnel: func(ctx context.Context) (Endpoints, func() error, error) {
			return Endpoints{Connect: "http://127.0.0.1:1"}, func() error { <-ctx.Done(); return nil }, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "journalctl -u sbx-fc-serve") {
		t.Fatalf("got %v", err)
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)

	return hex.EncodeToString(h[:])
}

func TestEnsureInstallsByContentAndRestartsOnlyOnChange(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sbx")
	payload := []byte("\x7fELF pretend linux sbx")

	if err := os.WriteFile(bin, payload, 0o755); err != nil {
		t.Fatal(err)
	}

	find := func(context.Context) (string, error) { return bin, nil }

	t.Run("same bytes already there, daemon active with this spec: nothing is copied or restarted", func(t *testing.T) {
		r := (&fakeRunner{}).on("limactl list", runningLima, nil)
		m := limaManager(t, r)

		tok, _ := m.Token()
		r.onContains("cat /etc/sbx-fc/spec", DaemonSpec("SBX_CONNECT_TOKEN="+tok+"\nHOME=/root\n", ServeArgv(nil, false))+"\n", nil).
			on("ssh", sum(payload)+"  /usr/local/bin/sbx\n", nil)

		if err := m.Ensure(context.Background(), EnsureOptions{Binary: find}); err != nil {
			t.Fatal(err)
		}

		all := strings.Join(r.lines(), "\n")
		if strings.Contains(all, "cat > /usr/local/bin/sbx.new") || strings.Contains(all, "systemd-run") {
			t.Fatalf("did work it did not need to:\n%s", all)
		}

		if !strings.Contains(all, "systemctl is-active --quiet sbx-fc-serve && cat /etc/sbx-fc/spec") {
			t.Fatalf("never checked the daemon:\n%s", all)
		}
	})

	t.Run("new bytes: copied over stdin, daemon restarted", func(t *testing.T) {
		r := (&fakeRunner{}).on("limactl list", runningLima, nil).on("ssh", "", nil)
		m := limaManager(t, r)

		if err := m.Ensure(context.Background(), EnsureOptions{Binary: find, OSBKey: "osbk", Serve: []string{"--idle", "2m"}}); err != nil {
			t.Fatal(err)
		}

		all := strings.Join(r.lines(), "\n")
		for _, w := range []string{
			"cat > /usr/local/bin/sbx.new && chmod 0755 /usr/local/bin/sbx.new && mv -f /usr/local/bin/sbx.new /usr/local/bin/sbx",
			"systemctl stop sbx-fc-serve",
			"systemd-run --quiet --unit=sbx-fc-serve --collect -p Restart=on-failure -p EnvironmentFile=/etc/sbx-fc/env",
			`'\''/usr/local/bin/sbx'\'' '\''serve'\'' '\''--provider'\'' '\''firecracker'\''`,
			`'\''--connect-addr'\'' '\''127.0.0.1:22980'\'' '\''--idle'\'' '\''2m'\''`,
		} {
			if !strings.Contains(all, w) {
				t.Errorf("no %q in\n%s", w, all)
			}
		}

		if strings.Contains(all, "--osb-addr") {
			t.Error("the in-VM daemon was given an OpenSandbox listener nobody asked for")
		}

		if !slices.Contains(r.stdin, string(payload)) {
			t.Error("the binary was not what went over stdin")
		}

		// Secrets travel on stdin into a 0600 file, never on a command line.
		tok, _ := m.Token()

		var env string

		for _, s := range r.stdin {
			if strings.HasPrefix(s, "SBX_CONNECT_TOKEN=") {
				env = s
			}
		}

		// HOME: a systemd unit has none, and the daemon must find the state the redirected
		// commands (sudo -H, so /root) write - without it the daemon exits at start.
		if env != "SBX_CONNECT_TOKEN="+tok+"\nHOME=/root\nSBX_OSB_KEY=osbk\n" {
			t.Errorf("env file = %q", env)
		}

		if strings.Contains(all, tok) || strings.Contains(all, "osbk") {
			t.Error("a secret appeared in an argv")
		}
	})
}

func TestTokenIsStableAndPrivate(t *testing.T) {
	m := limaManager(t, &fakeRunner{})

	a, err := m.Token()
	if err != nil {
		t.Fatal(err)
	}

	b, _ := m.Token()
	if a != b || len(a) != 64 {
		t.Fatalf("token %q then %q", a, b)
	}

	fi, err := os.Stat(filepath.Join(m.StateDir, "token-sbx-fc"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi.Mode(), err)
	}
}

func TestTunnelForwardsOnlyTheTwoControlPorts(t *testing.T) {
	got := strings.Join(TunnelArgv(transport{sshConfig: "/c", sshHost: "lima-sbx-fc"}, 50001, 50002), " ")

	// ControlMaster=no, ControlPath=none: lima and colima both write ControlMaster auto +
	// ControlPersist yes, and through that mux `ssh -N -L` hands its forwards to the master and
	// exits at once - which the front reads as the tunnel closing.
	want := "ssh -F /c -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 " +
		"-o ServerAliveCountMax=3 -N -L 127.0.0.1:50001:127.0.0.1:22980 -L 127.0.0.1:50002:127.0.0.1:22981 lima-sbx-fc"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestProviderKind(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }

	cases := []struct {
		args []string
		env  string
		want string
	}{
		{[]string{"demo", "--provider", "firecracker"}, "", "firecracker"},
		{[]string{"demo", "--provider=firecracker"}, "docker", "firecracker"},
		{[]string{"demo", "-provider", "docker"}, "firecracker", "docker"},
		{[]string{"demo"}, "firecracker", "firecracker"},
		// The alias provider.For accepts is the same provider here too.
		{[]string{"demo", "--provider", "fc"}, "", "firecracker"},
		{[]string{"demo"}, "fc", "firecracker"},
		{[]string{"demo", "--provider=k8s"}, "", "kubernetes"},
		// After -- it is the sandboxed command's flag, not sbx's.
		{[]string{"demo", "db", "--", "tool", "--provider", "firecracker"}, "", ""},
	}

	for _, c := range cases {
		if got := ProviderKind(c.args, env(c.env)); got != c.want {
			t.Errorf("%q env=%q: %q, want %q", c.args, c.env, got, c.want)
		}
	}

	none := env("")
	if !Wants("create", []string{"x", "--provider", "firecracker"}, none) || !Wants("serve", []string{"--provider=firecracker"}, none) {
		t.Error("create and serve with firecracker must be handled")
	}

	if !Wants("create", []string{"x", "--provider", "fc"}, none) || !Wants("serve", []string{"--provider=fc"}, none) {
		t.Error("--provider fc skipped the helper VM")
	}

	if Wants("create", []string{"x"}, none) || Wants("ui", []string{"--provider", "firecracker"}, none) ||
		Wants("with", []string{"--provider", "firecracker"}, none) {
		t.Error("docker commands, and the ones that act on this machine, must not be redirected")
	}
}

func exitErr(t *testing.T, code int) error {
	t.Helper()

	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatal("sh exited 0")
	}

	return err
}

func TestRedirectRunsTheSameCommandInTheVM(t *testing.T) {
	r := (&fakeRunner{}).on("limactl list", runningLima, nil).on("ssh", "export PG_PORT=45432\n", nil)
	m := limaManager(t, r)

	var stdout, stderr bytes.Buffer

	code := m.redirect(context.Background(), "dev", "env", []string{"demo", "--provider", "firecracker"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}

	if stdout.String() != "export PG_PORT=45432\n" {
		t.Fatalf("stdout = %q: it must be exactly the VM's, so eval works", stdout.String())
	}

	last := r.calls[len(r.calls)-1]
	if last[0] != "ssh" || !strings.Contains(last[len(last)-1],
		"exec sudo -H 'env' 'SBX_PROVIDER_KIND=firecracker' '/usr/local/bin/sbx' 'env' 'demo' '--provider' 'firecracker'") {
		t.Fatalf("ran %q", last)
	}

	if strings.Contains(strings.Join(r.lines(), "\n"), "limactl start") {
		t.Fatal("started a VM that was already running")
	}
}

func TestRedirectPassesTheExitCodeThrough(t *testing.T) {
	r := (&fakeRunner{}).on("limactl list", runningLima, nil).on("ssh", "", exitErr(t, 3))
	m := limaManager(t, r)

	if code := m.redirect(context.Background(), "dev", "exec", []string{"demo", "db", "false"},
		strings.NewReader(""), io.Discard, io.Discard); code != 3 {
		t.Fatalf("exit %d, want the command's own 3", code)
	}
}

func TestRedirectStartsAStoppedVMFirst(t *testing.T) {
	var mu sync.Mutex

	started := false
	r := &fakeRunner{}
	r.on("limactl list", `{"name":"sbx-fc","status":"Stopped"}`, nil)

	m := limaManager(t, r)
	bin := filepath.Join(t.TempDir(), "sbx")
	_ = os.WriteFile(bin, []byte("x"), 0o755)
	t.Setenv("SBX_EXECD_BINARY", bin) // or Ensure would cross-compile this module

	// After start, the listing says running.
	m.Run = runnerFunc{r, func(c Cmd) {
		mu.Lock()
		defer mu.Unlock()

		if strings.HasPrefix(strings.Join(c.Argv, " "), "limactl start") && !started {
			started = true
			r.answers = append([]answer{{prefix: "limactl list", out: runningLima}}, r.answers...)
		}
	}}

	// create, not list: a read-only command reports a stopped VM rather than starting it.
	code := m.redirect(context.Background(), "dev", "create", []string{"demo"}, strings.NewReader(""), io.Discard, io.Discard)

	all := strings.Join(r.lines(), "\n")
	if code != 0 || !strings.Contains(all, "limactl start --tty=false sbx-fc") || !strings.Contains(all, "systemd-run") {
		t.Fatalf("a stopped VM was not started (exit %d):\n%s", code, all)
	}
}

// runnerFunc lets a test react to a command before the recorder answers it.
type runnerFunc struct {
	*fakeRunner
	before func(Cmd)
}

func (r runnerFunc) Run(ctx context.Context, c Cmd) error {
	r.before(c)

	return r.fakeRunner.Run(ctx, c)
}

// The in-VM provider builds each rootfs through a docker engine and needs /dev/kvm; the VM is
// checked for both, and docker installed when missing, before any sbx is put in it.
func TestEnsureProvisionsDockerAndChecksKVMFirst(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sbx")
	_ = os.WriteFile(bin, []byte("x"), 0o755)

	r := (&fakeRunner{}).on("limactl list", runningLima, nil).on("ssh", "", nil)
	m := limaManager(t, r)

	if err := m.Ensure(context.Background(), EnsureOptions{Binary: func(context.Context) (string, error) { return bin, nil }}); err != nil {
		t.Fatal(err)
	}

	all := r.lines()

	prov := slices.IndexFunc(all, func(l string) bool { return strings.Contains(l, "apt-get install -y -q docker.io e2fsprogs") })
	inst := slices.IndexFunc(all, func(l string) bool { return strings.Contains(l, "cat > /usr/local/bin/sbx.new") })

	if prov < 0 || inst < 0 || prov > inst {
		t.Fatalf("provisioning at %d, install at %d:\n%s", prov, inst, strings.Join(all, "\n"))
	}

	if !strings.Contains(all[prov], "[ -c /dev/kvm ]") {
		t.Error("provisioning does not check /dev/kvm inside the VM")
	}
}

func TestEnsureSaysWhenNestedVirtDidNotTakeEffect(t *testing.T) {
	r := (&fakeRunner{}).on("limactl list", runningLima, nil).on("ssh", "", exitErr(t, 3))
	m := limaManager(t, r)

	err := m.Ensure(context.Background(), EnsureOptions{Binary: func(context.Context) (string, error) { return "/x", nil }})
	if err == nil || !strings.Contains(err.Error(), "/dev/kvm") {
		t.Fatalf("got %v", err)
	}
}

// The in-VM daemon is restarted when what it runs with changed, not only when the binary did: a
// new --idle (or --osb-key, --only, token, control port) against the same bytes used to leave the
// old daemon running with the old flags.
func TestEnsureRestartsTheDaemonWhenItsFlagsChange(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sbx")
	payload := []byte("\x7fELF pretend linux sbx")

	if err := os.WriteFile(bin, payload, 0o755); err != nil {
		t.Fatal(err)
	}

	find := func(context.Context) (string, error) { return bin, nil }

	r := (&fakeRunner{}).on("limactl list", runningLima, nil).
		onContains("cat /etc/sbx-fc/spec", "the-spec-it-was-started-with\n", nil).
		on("ssh", sum(payload)+"  /usr/local/bin/sbx\n", nil)
	m := limaManager(t, r)

	if err := m.Ensure(context.Background(), EnsureOptions{Binary: find, SetDaemon: true,
		OSBKey: "k1", Serve: []string{"--idle", "2m"}}); err != nil {
		t.Fatal(err)
	}

	all := strings.Join(r.lines(), "\n")
	if !strings.Contains(all, "systemd-run") || !strings.Contains(all, `'\''--idle'\'' '\''2m'\''`) {
		t.Fatalf("a daemon running with other flags was not restarted with these:\n%s", all)
	}

	// A redirected command's Ensure has no opinion, and keeps what serve set.
	r.calls, r.stdin = nil, nil

	if err := m.Ensure(context.Background(), EnsureOptions{Binary: find}); err != nil {
		t.Fatal(err)
	}

	all = strings.Join(r.lines(), "\n")
	if !strings.Contains(all, `'\''--idle'\'' '\''2m'\''`) || !slices.ContainsFunc(r.stdin, func(s string) bool {
		return strings.Contains(s, "SBX_OSB_KEY=k1\n")
	}) {
		t.Fatalf("a redirected Ensure dropped the daemon's key or flags:\nstdin %q\n%s", r.stdin, all)
	}

	if fi, err := os.Stat(m.daemonConfigPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("daemon config: %v %v", fi, err)
	}
}

// `sbx list/env/logs --provider firecracker` against a stopped helper VM say so; they do not boot
// a 2 GiB VM to report that nothing is running in it.
func TestReadOnlyCommandsDoNotStartTheHelperVM(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		code int
	}{{"list", 0}, {"env", 1}, {"logs", 1}} {
		r := (&fakeRunner{}).on("limactl list", `{"name":"sbx-fc","status":"Stopped"}`, nil)
		m := limaManager(t, r)

		var stderr bytes.Buffer

		code := m.redirect(context.Background(), "dev", tc.cmd, []string{"demo"}, strings.NewReader(""), io.Discard, &stderr)

		if code != tc.code || !strings.Contains(stderr.String(), "is stopped") {
			t.Errorf("%s: exit %d, stderr %q", tc.cmd, code, stderr.String())
		}

		if all := strings.Join(r.lines(), "\n"); strings.Contains(all, "limactl start") || strings.Contains(all, "ssh") {
			t.Errorf("%s started or entered the helper VM:\n%s", tc.cmd, all)
		}
	}
}
