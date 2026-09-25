package fchost

// The helper VM's lifecycle, and what sbx puts inside it.
//
// Created on first use, started on demand, stopped and removed only when asked. Inside it runs
// the linux build of this same sbx, as `sbx serve --provider firecracker`, under systemd so it
// outlives the ssh session that started it. The host never talks to Firecracker: it talks to
// that daemon, through the connect tunnel sbx already has (see front.go).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// In-VM constants. Loopback inside the VM: nothing but the host's ssh forward reaches them.
//
// The two control ports sit OUTSIDE the sandbox ranges (public 20000-22559, backing 30000-32559):
// WSL forwards the VM's loopback to the host at the same numbers, and the in-VM daemon binds every
// sandbox's public port there too, so a control port inside the public range is a slot that can
// never be served.
const (
	GuestConnectPort = 22980
	GuestOSBPort     = 22981
	guestBinary      = "/usr/local/bin/sbx"
	guestUnit        = "sbx-fc-serve"
	guestEnvFile     = "/etc/sbx-fc/env"
)

// Cmd is one external command.
type Cmd struct {
	Argv           []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Runner executes commands. The real one is os/exec; tests hand in a recorder, which is how
// every lima, colima and wsl invocation is checked without a VM.
type Runner interface {
	// Output runs to completion and returns stdout.
	Output(ctx context.Context, c Cmd) (string, error)

	// Run runs to completion with the streams in c.
	Run(ctx context.Context, c Cmd) error

	// Start runs in the background until ctx ends; wait returns when it exits.
	Start(ctx context.Context, c Cmd) (wait func() error, err error)
}

type execRunner struct{}

func (execRunner) cmd(ctx context.Context, c Cmd) *exec.Cmd {
	x := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	x.Stdin, x.Stdout, x.Stderr = c.Stdin, c.Stdout, c.Stderr

	return x
}

func (r execRunner) Output(ctx context.Context, c Cmd) (string, error) {
	x := r.cmd(ctx, c)
	x.Stdout = nil

	var stderr strings.Builder
	if x.Stderr == nil {
		x.Stderr = &stderr
	}

	out, err := x.Output()
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%s: %w: %s", c.Argv[0], err, strings.TrimSpace(stderr.String()))
	}

	return string(out), err
}

func (r execRunner) Run(ctx context.Context, c Cmd) error { return r.cmd(ctx, c).Run() }

func (r execRunner) Start(ctx context.Context, c Cmd) (func() error, error) {
	x := r.cmd(ctx, c)
	if err := x.Start(); err != nil {
		return nil, err
	}

	return x.Wait, nil
}

// Manager drives one helper VM.
type Manager struct {
	Driver   Driver
	Config   Config
	Run      Runner
	Out      io.Writer // progress for a person; never parsed
	StateDir string    // host-side files: the ssh config colima prints, the connect token
}

// NewManager is the manager for the helper Detect chose, sized from the environment.
func NewManager(helper string, out io.Writer) (*Manager, error) {
	d, err := DriverFor(helper)
	if err != nil {
		return nil, err
	}

	cfg, err := ConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	return &Manager{Driver: d, Config: cfg, Run: execRunner{}, Out: out, StateDir: filepath.Join(home, ".sbx", "fc")}, nil
}

func (m *Manager) say(format string, a ...any) {
	if m.Out != nil {
		fmt.Fprintf(m.Out, "sbx fc: "+format+"\n", a...)
	}
}

// Status is what the tool reports. Read-only.
func (m *Manager) Status(ctx context.Context) (State, error) {
	if err := m.Config.Validate(); err != nil {
		return "", err
	}

	out, err := m.Run.Output(ctx, Cmd{Argv: m.Driver.ListArgv(m.Config)})
	if err != nil {
		// limactl with no instances at all prints nothing and exits 0; anything else failing
		// means the tool itself is broken, and saying "absent" would send `start` at it.
		return "", fmt.Errorf("asking %s for the helper VM: %w", m.Driver.Name(), err)
	}

	return m.Driver.ParseState(out, m.Config.Name)
}

// Start creates the VM if it does not exist and starts it if it is stopped.
func (m *Manager) Start(ctx context.Context) error {
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}

	if st == Running {
		return nil
	}

	if m.Driver.Name() == "wsl" && (m.Config.CPUs != 2 || m.Config.MemoryGiB != 2) {
		m.say("WSL sizes every distro together, from [wsl2] processors= and memory= in .wslconfig; " +
			"the CPU and memory asked for here are not applied")
	}

	return m.guardDockerContext(ctx, func() error {
		if st == Absent {
			if argv := m.Driver.CreateArgv(m.Config); argv != nil {
				m.say("creating helper VM %s with %s (%d CPU, %g GiB, %d GiB disk)",
					m.Config.Name, m.Driver.Name(), m.Config.CPUs, m.Config.MemoryGiB, m.Config.DiskGiB)

				if err := m.loud(ctx, argv); err != nil {
					return fmt.Errorf("creating the helper VM: %w", err)
				}
			}
		}

		m.say("starting helper VM %s", m.Config.Name)

		if err := m.loud(ctx, m.Driver.StartArgv(m.Config)); err != nil {
			return fmt.Errorf("starting the helper VM: %w (see `sbx fc vm status`)", err)
		}

		return nil
	})
}

// Stop stops a running VM. Its disk, the installed binary and every snapshot stay.
func (m *Manager) Stop(ctx context.Context) error {
	st, err := m.Status(ctx)
	if err != nil || st != Running {
		return err
	}

	return m.loud(ctx, m.Driver.StopArgv(m.Config))
}

// Remove deletes the VM and everything in it: every microVM sandbox it held goes with it.
func (m *Manager) Remove(ctx context.Context) error {
	st, err := m.Status(ctx)
	if err != nil || st == Absent {
		return err
	}

	return m.loud(ctx, m.Driver.RemoveArgv(m.Config))
}

// loud runs a lifecycle command with its output where a person can see it: a VM create takes
// a minute, and a minute of silence reads as a hang.
func (m *Manager) loud(ctx context.Context, argv []string) error {
	return m.Run.Run(ctx, Cmd{Argv: argv, Stdout: m.Out, Stderr: m.Out})
}

// guardDockerContext puts the global docker context back after a colima start.
//
// colima switches it to its own profile on start, and that context is global: every docker
// command in every terminal on this Mac would silently go to the helper VM afterwards. It runs
// even when the start failed, because a half-started colima can have switched it already.
func (m *Manager) guardDockerContext(ctx context.Context, fn func() error) error {
	if !m.Driver.SwitchesDockerContext() {
		return fn()
	}

	show := Cmd{Argv: []string{"docker", "context", "show"}}

	prev, perr := m.Run.Output(ctx, show)
	prev = strings.TrimSpace(prev)

	err := fn()

	if perr != nil || prev == "" {
		return err // no docker CLI here, so no context to protect
	}

	now, _ := m.Run.Output(ctx, show)
	if strings.TrimSpace(now) != prev {
		if rerr := m.Run.Run(ctx, Cmd{Argv: []string{"docker", "context", "use", prev}, Stdout: io.Discard, Stderr: m.Out}); rerr != nil {
			return errors.Join(err, fmt.Errorf("colima switched the docker context and putting it back failed - "+
				"run `docker context use %s`: %w", prev, rerr))
		}
	}

	return err
}

// transport finds how to reach the running VM.
func (m *Manager) transport(ctx context.Context) (transport, error) {
	switch m.Driver.Name() {
	case "lima":
		out, err := m.Run.Output(ctx, Cmd{Argv: m.Driver.ListArgv(m.Config)})
		if err != nil {
			return transport{}, err
		}

		st, line, err := jsonLinesState(out, m.Config.Name)
		if err != nil {
			return transport{}, err
		}

		if st != Running || line.SSHConfigFile == "" {
			return transport{}, errNotRunning
		}

		return transport{sshConfig: line.SSHConfigFile, sshHost: "lima-" + m.Config.Name}, nil
	case "colima":
		out, err := m.Run.Output(ctx, Cmd{Argv: m.Driver.SSHConfigArgv(m.Config)})
		if err != nil {
			return transport{}, fmt.Errorf("reading the helper VM's ssh config: %w", err)
		}

		if err := os.MkdirAll(m.StateDir, 0o700); err != nil {
			return transport{}, err
		}

		path := filepath.Join(m.StateDir, "ssh_config-"+m.Config.Name)
		if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
			return transport{}, err
		}

		return transport{sshConfig: path, sshHost: "colima-" + m.Config.Name}, nil
	default:
		return transport{}, nil
	}
}

// Shell is the command that runs argv inside the VM, as root, from dir.
func (m *Manager) Shell(ctx context.Context, dir string, tty bool, argv []string) ([]string, error) {
	t, err := m.transport(ctx)
	if err != nil {
		return nil, err
	}

	return m.Driver.ShellArgv(m.Config, t, dir, tty, true, argv), nil
}

func (m *Manager) inVM(ctx context.Context, stdin io.Reader, argv ...string) (string, error) {
	sh, err := m.Shell(ctx, "/", false, argv)
	if err != nil {
		return "", err
	}

	return m.Run.Output(ctx, Cmd{Argv: sh, Stdin: stdin})
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// Install puts file at /usr/local/bin/sbx in the VM unless the same bytes are already there.
// Compared by content, never version: a dev build is "dev" every time (DECISIONS: keyed by
// content, never by age). Written to a temporary name and renamed, so the running daemon's
// executable is replaced rather than truncated under it.
func (m *Manager) Install(ctx context.Context, file string) (changed bool, err error) {
	sum, err := fileSum(file)
	if err != nil {
		return false, fmt.Errorf("the linux sbx binary for the helper VM: %w", err)
	}

	have, _ := m.inVM(ctx, nil, "sh", "-c", "sha256sum "+guestBinary+" 2>/dev/null || true")
	if strings.HasPrefix(strings.TrimSpace(have), sum) {
		return false, nil
	}

	f, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer f.Close()

	m.say("installing %s into %s", filepath.Base(file), m.Config.Name)

	_, err = m.inVM(ctx, f, "sh", "-c",
		"cat > "+guestBinary+".new && chmod 0755 "+guestBinary+".new && mv -f "+guestBinary+".new "+guestBinary)
	if err != nil {
		return false, fmt.Errorf("copying sbx into the helper VM: %w", err)
	}

	return true, nil
}

// InstallRelease fetches the published linux asset inside the VM, for a release build on a Mac
// with no go toolchain - the case the host has no linux binary to copy. Same asset name and
// checksum file as scripts/install.sh.
func (m *Manager) InstallRelease(ctx context.Context, version string) error {
	tag := version
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}

	script := `set -eu
case "$(uname -m)" in x86_64|amd64) a=amd64;; aarch64|arm64) a=arm64;; *) echo "unsupported arch $(uname -m)" >&2; exit 1;; esac
asset="sbx_$1_linux_$a"; base="https://github.com/aryanmehrotra/sbx/releases/download/$1"
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
curl -fsSL "$base/$asset" -o "$t/sbx"
curl -fsSL "$base/SHA256SUMS" -o "$t/sums"
want=$(grep " $asset\$" "$t/sums" | cut -d' ' -f1)
[ -n "$want" ] && [ "$want" = "$(sha256sum "$t/sbx" | cut -d' ' -f1)" ] || { echo "checksum mismatch for $asset" >&2; exit 1; }
install -m 0755 "$t/sbx" ` + guestBinary + `.new && mv -f ` + guestBinary + `.new ` + guestBinary

	m.say("fetching sbx %s into %s", tag, m.Config.Name)

	if _, err := m.inVM(ctx, nil, "sh", "-c", script, "sbx-install", tag); err != nil {
		return fmt.Errorf("fetching sbx %s inside the helper VM: %w - set SBX_EXECD_BINARY to a linux sbx build to copy one in instead", tag, err)
	}

	return nil
}

// Token is the connect token the host and the in-VM daemon share, made once and kept 0600.
func (m *Manager) Token() (string, error) {
	path := filepath.Join(m.StateDir, "token-"+m.Config.Name)

	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	tok := hex.EncodeToString(buf)

	if err := os.MkdirAll(m.StateDir, 0o700); err != nil {
		return "", err
	}

	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

// DaemonOptions is what the in-VM `sbx serve` is started with.
type DaemonOptions struct {
	Token  string
	OSBKey string

	// Serve is extra `sbx serve` arguments passed through: --idle, --ready, --refresh, --only,
	// --osb-host-paths. The addresses and the provider are this package's to decide.
	Serve []string

	// Restart forces a restart of a daemon already running - after a new binary went in.
	Restart bool
}

// ServeArgv is the in-VM daemon's command line.
func ServeArgv(extra []string) []string {
	return append([]string{
		guestBinary, "serve", "--provider", "firecracker",
		"--connect-addr", "127.0.0.1:" + strconv.Itoa(GuestConnectPort),
		"--osb-addr", "127.0.0.1:" + strconv.Itoa(GuestOSBPort),
	}, extra...)
}

// StartDaemon runs `sbx serve --provider firecracker` in the VM as a transient systemd unit:
// supervised, restarted on failure, and with its secrets in a root-only environment file
// rather than on a command line anyone in the VM can read with ps.
func (m *Manager) StartDaemon(ctx context.Context, opt DaemonOptions) error {
	// HOME=/root: a transient unit has no HOME, and without one the provider has no state
	// directory and the daemon exits at start. /root, because every redirected command runs
	// as `sudo -H`, and the daemon must read the state those commands write.
	env := "SBX_CONNECT_TOKEN=" + opt.Token + "\nHOME=/root\n"
	if opt.OSBKey != "" {
		env += "SBX_OSB_KEY=" + opt.OSBKey + "\n"
	}

	if _, err := m.inVM(ctx, strings.NewReader(env), "sh", "-c",
		"umask 077 && mkdir -p "+filepath.Dir(guestEnvFile)+" && cat > "+guestEnvFile); err != nil {
		return fmt.Errorf("writing the helper VM daemon's environment: %w", err)
	}

	if !opt.Restart {
		if _, err := m.inVM(ctx, nil, "systemctl", "is-active", "--quiet", guestUnit); err == nil {
			return nil
		}
	}

	q := make([]string, 0, len(ServeArgv(opt.Serve)))
	for _, a := range ServeArgv(opt.Serve) {
		q = append(q, quote(a))
	}

	script := "systemctl stop " + guestUnit + " 2>/dev/null; systemctl reset-failed " + guestUnit + " 2>/dev/null; " +
		"exec systemd-run --quiet --unit=" + guestUnit + " --collect -p Restart=on-failure " +
		"-p EnvironmentFile=" + guestEnvFile + " " + strings.Join(q, " ")

	if _, err := m.inVM(ctx, nil, "sh", "-c", script); err != nil {
		return fmt.Errorf("starting sbx serve in the helper VM: %w - is systemd running there? "+
			"(WSL: add [boot] systemd=true to /etc/wsl.conf in the distro)", err)
	}

	return nil
}

// Endpoints is where the host reaches the in-VM daemon's connect and OSB listeners.
type Endpoints struct {
	Connect, OSB string
}

// Tunnel forwards the two in-VM control ports to free host ports over ssh, until ctx ends.
// WSL needs no tunnel: its own localhost forwarding already puts the VM's loopback on the
// host's, at the same numbers.
func (m *Manager) Tunnel(ctx context.Context) (Endpoints, func() error, error) {
	if m.Driver.NativeForwarding() {
		return Endpoints{
			Connect: fmt.Sprintf("http://127.0.0.1:%d", GuestConnectPort),
			OSB:     fmt.Sprintf("http://127.0.0.1:%d", GuestOSBPort),
		}, func() error { <-ctx.Done(); return nil }, nil
	}

	t, err := m.transport(ctx)
	if err != nil {
		return Endpoints{}, nil, err
	}

	pc, err := freePort()
	if err != nil {
		return Endpoints{}, nil, err
	}

	po, err := freePort()
	if err != nil {
		return Endpoints{}, nil, err
	}

	argv := TunnelArgv(t, pc, po)

	wait, err := m.Run.Start(ctx, Cmd{Argv: argv, Stderr: m.Out})
	if err != nil {
		return Endpoints{}, nil, fmt.Errorf("opening the ssh tunnel to the helper VM: %w", err)
	}

	return Endpoints{
		Connect: fmt.Sprintf("http://127.0.0.1:%d", pc),
		OSB:     fmt.Sprintf("http://127.0.0.1:%d", po),
	}, wait, nil
}

// TunnelArgv is the ssh local forward. Its own connection, never the config's ControlMaster:
// through lima's or colima's persistent mux, `ssh -N -L` registers the forwards on the master and
// exits at once, which reads as the tunnel closing. ExitOnForwardFailure so a port that could not be bound
// is an exit, not a tunnel that silently carries nothing; keepalives so a sleeping laptop's dead
// connection is noticed.
func TunnelArgv(t transport, hostConnect, hostOSB int) []string {
	return []string{
		"ssh", "-F", t.sshConfig, "-o", "LogLevel=ERROR", "-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-N",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", hostConnect, GuestConnectPort),
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", hostOSB, GuestOSBPort),
		t.sshHost,
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()

	return ln.Addr().(*net.TCPAddr).Port, nil
}

// StatusReport is `sbx fc vm status`, for a person or --json.
type StatusReport struct {
	Name   string  `json:"name"`
	Driver string  `json:"driver"`
	State  State   `json:"state"`
	CPUs   int     `json:"cpus"`
	Memory float64 `json:"memoryGiB"`
	Disk   int     `json:"diskGiB"`
	Daemon string  `json:"daemon,omitempty"` // systemd's word for the in-VM sbx serve, when running
}

// Report collects StatusReport. Read-only.
func (m *Manager) Report(ctx context.Context) (StatusReport, error) {
	st, err := m.Status(ctx)

	r := StatusReport{Name: m.Config.Name, Driver: m.Driver.Name(), State: st,
		CPUs: m.Config.CPUs, Memory: m.Config.MemoryGiB, Disk: m.Config.DiskGiB}

	if err == nil && st == Running {
		out, _ := m.inVM(ctx, nil, "systemctl", "is-active", guestUnit)
		r.Daemon = strings.TrimSpace(out)
	}

	return r, err
}

// JSON is the report as one line, for scripts.
func (r StatusReport) JSON() string {
	b, _ := json.Marshal(r)

	return string(b)
}

// provisionScript is what the VM needs before the firecracker provider can run in it: /dev/kvm
// (checked, never faked - exit 3 says nested virtualisation did not take effect), and a docker
// engine plus e2fsprogs, which is how the provider turns an image into an ext4 root. Installed
// only when missing, so a warm VM pays one `command -v` and one `docker info`.
const provisionScript = `set -e
[ -c /dev/kvm ] || exit 3
if ! command -v docker >/dev/null 2>&1 || ! command -v mkfs.ext4 >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || { echo "the helper VM has no docker and no apt-get to install it" >&2; exit 4; }
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q >/dev/null && apt-get install -y -q docker.io e2fsprogs >/dev/null
fi
systemctl enable --now docker >/dev/null 2>&1 || true
docker info >/dev/null`

// Provision makes the VM able to run microVMs, or says precisely why it cannot.
func (m *Manager) Provision(ctx context.Context) error {
	_, err := m.inVM(ctx, nil, "sh", "-c", provisionScript)

	var exit *exec.ExitError

	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 3:
		return fmt.Errorf("no /dev/kvm inside the helper VM %s: nested virtualisation did not take effect. "+
			"`sbx fc vm rm --yes` and start again; if it persists, check `sbx fc backend` and that %s supports "+
			"nested virtualisation on this machine", m.Config.Name, m.Driver.Name())
	default:
		return fmt.Errorf("preparing the helper VM (docker, e2fsprogs): %w", err)
	}
}
