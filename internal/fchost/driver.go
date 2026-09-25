package fchost

// The three tools that can run a Linux VM with /dev/kvm in it, reduced to the argv sbx hands
// them. Tools are shelled out, never linked (DECISIONS: "Tunnels are shelled out") - the same
// reason as there: they already solve the hard part, and go.mod stays at zero requires.
//
// Every command names the instance explicitly. colima's default profile is "default", which is
// somebody's docker; a colima command that forgot --profile would stop it.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Config is the helper VM's shape.
type Config struct {
	Name      string
	CPUs      int
	MemoryGiB float64
	DiskGiB   int
}

// DefaultName is the helper VM's name in every tool.
const DefaultName = "sbx-fc"

var nameRE = regexp.MustCompile(`^sbx-[a-z0-9][a-z0-9-]*$`)

// Validate refuses a name sbx did not choose. The "sbx-" prefix is the guarantee that `sbx fc
// vm rm` can never be pointed at colima's default profile, the osb one, or anyone else's VM.
func (c Config) Validate() error {
	if !nameRE.MatchString(c.Name) {
		return fmt.Errorf("helper VM name %q: it must start with sbx- and be lowercase letters, "+
			"digits and dashes - sbx only ever manages a VM it named", c.Name)
	}

	if c.CPUs < 1 {
		return fmt.Errorf("helper VM needs at least 1 CPU, got %d", c.CPUs)
	}

	// Firecracker, the daemon and one 128 MiB guest measured fine in 2 GiB; under 1 GiB the
	// guest kernel alone starts competing with the page cache the snapshot restores from.
	if c.MemoryGiB < 1 {
		return fmt.Errorf("helper VM needs at least 1 GiB of memory, got %g", c.MemoryGiB)
	}

	if c.DiskGiB < 10 {
		return fmt.Errorf("helper VM needs at least 10 GiB of disk (a snapshot is the size of the "+
			"guest's memory), got %d", c.DiskGiB)
	}

	return nil
}

// ConfigFromEnv reads SBX_FC_VM_NAME/_CPUS/_MEMORY (GiB)/_DISK (GiB) over the defaults: 2 CPU
// and 2 GiB, the size the spike measured.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{Name: DefaultName, CPUs: 2, MemoryGiB: 2, DiskGiB: 20}

	if v := getenv("SBX_FC_VM_NAME"); v != "" {
		c.Name = v
	}

	for _, f := range []struct {
		key string
		set func(string) error
	}{
		{"SBX_FC_VM_CPUS", func(v string) (err error) { c.CPUs, err = strconv.Atoi(v); return }},
		{"SBX_FC_VM_MEMORY", func(v string) (err error) { c.MemoryGiB, err = strconv.ParseFloat(v, 64); return }},
		{"SBX_FC_VM_DISK", func(v string) (err error) { c.DiskGiB, err = strconv.Atoi(v); return }},
	} {
		if v := getenv(f.key); v != "" {
			if err := f.set(v); err != nil {
				return c, fmt.Errorf("%s=%q is not a number", f.key, v)
			}
		}
	}

	return c, nil
}

// State is what the tool says about the VM.
type State string

const (
	Absent  State = "absent"
	Stopped State = "stopped"
	Running State = "running"
)

// transport is how to reach a running VM: its ssh config for lima and colima, nothing for WSL,
// whose own localhost forwarding and wsl.exe are the transport.
type transport struct {
	sshConfig string
	sshHost   string
}

// Driver is one VM tool.
type Driver interface {
	Name() string

	ListArgv(c Config) []string
	ParseState(out, name string) (State, error)

	// CreateArgv is nil for a tool whose start creates.
	CreateArgv(c Config) []string
	StartArgv(c Config) []string
	StopArgv(c Config) []string
	RemoveArgv(c Config) []string

	// SSHConfigArgv prints an ssh config for the VM; nil when the tool has none to print
	// (lima keeps it in a file the listing names) or no ssh at all (wsl).
	SSHConfigArgv(c Config) []string

	// ShellArgv runs argv inside the VM, in dir when it exists there, as root when asked.
	ShellArgv(c Config, t transport, dir string, tty, root bool, argv []string) []string

	// SwitchesDockerContext is true for a tool whose start may repoint the global docker
	// context, which then has to be put back.
	SwitchesDockerContext() bool

	// NativeForwarding is true when the tool itself forwards every guest localhost port to the
	// host (WSL2), so the connect tunnel must not bind the same numbers a second time.
	NativeForwarding() bool
}

// DriverFor returns the driver Detect chose.
func DriverFor(helper string) (Driver, error) {
	switch helper {
	case "lima":
		return lima{}, nil
	case "colima":
		return colima{}, nil
	case "wsl":
		return wsl{}, nil
	default:
		return nil, fmt.Errorf("no helper VM driver %q", helper)
	}
}

func gib(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// --- ssh, for lima and colima ---------------------------------------------------------------

// quote is POSIX single quoting: the one form in which no byte is special.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sshShell builds the command ssh sends. ssh joins its arguments with spaces and hands the
// result to the remote shell, so the argv has to arrive already quoted - which is why this goes
// through ssh rather than `colima ssh`, whose joining is not under sbx's control.
//
// The working directory is the host's: lima and colima mount $HOME at the same path, so a
// relative --spec from a project under $HOME means the same file inside. Outside $HOME the cd
// fails quietly and a relative path fails loudly, from sbx, naming the file.
func sshShell(t transport, dir string, tty, root bool, argv []string) []string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = quote(a)
	}

	run := "exec " + strings.Join(q, " ")
	if root {
		run = "exec sudo -H " + strings.Join(q, " ")
	}

	script := "cd " + quote(dir) + " 2>/dev/null || cd; " + run

	mode := "-T"
	if tty {
		mode = "-t"
	}

	return []string{"ssh", "-F", t.sshConfig, "-o", "LogLevel=ERROR", mode, t.sshHost, script}
}

type statusLine struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	SSHConfigFile string `json:"sshConfigFile"`
}

// jsonLinesState reads the one-object-per-line listing lima and colima both print.
func jsonLinesState(out, name string) (State, statusLine, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var s statusLine
		if json.Unmarshal([]byte(strings.TrimSpace(sc.Text())), &s) != nil || s.Name != name {
			continue
		}

		switch s.Status {
		case "Running":
			return Running, s, nil
		case "Stopped":
			return Stopped, s, nil
		default:
			return "", s, fmt.Errorf("helper VM %s is %q, which sbx will not start from - "+
				"`sbx fc vm rm` and start again", name, s.Status)
		}
	}

	return Absent, statusLine{}, nil
}

// --- lima -------------------------------------------------------------------------------------

type lima struct{}

func (lima) Name() string { return "lima" }

func (lima) ListArgv(Config) []string { return []string{"limactl", "list", "--json"} }

func (lima) ParseState(out, name string) (State, error) {
	s, _, err := jsonLinesState(out, name)

	return s, err
}

// limaPortRules ignores every guest port. Lima otherwise forwards whatever the guest binds on
// localhost, which would put each sandbox port on the Mac twice over - once by lima, once by
// the connect tunnel - and whichever bound first would win. Control traffic goes over ssh.
const limaPortRules = `.portForwards = [` +
	`{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true},` +
	`{"guestIP":"0.0.0.0","guestIPMustBeZero":false,"guestPortRange":[1,65535],"proto":"any","ignore":true}]`

func (lima) CreateArgv(c Config) []string {
	return []string{
		"limactl", "create", "--tty=false", "--name", c.Name,
		"--vm-type", "vz", "--nested-virt",
		"--cpus", strconv.Itoa(c.CPUs), "--memory", gib(c.MemoryGiB), "--disk", strconv.Itoa(c.DiskGiB),
		"--containerd", "none",
		"--set", limaPortRules,
		"template:ubuntu-24.04",
	}
}

func (lima) StartArgv(c Config) []string { return []string{"limactl", "start", "--tty=false", c.Name} }
func (lima) StopArgv(c Config) []string  { return []string{"limactl", "stop", "--tty=false", c.Name} }

func (lima) RemoveArgv(c Config) []string {
	return []string{"limactl", "delete", "--tty=false", "--force", c.Name}
}

func (lima) SSHConfigArgv(Config) []string { return nil }

func (lima) ShellArgv(_ Config, t transport, dir string, tty, root bool, argv []string) []string {
	return sshShell(t, dir, tty, root, argv)
}

func (lima) SwitchesDockerContext() bool { return false }
func (lima) NativeForwarding() bool      { return false }

// --- colima -----------------------------------------------------------------------------------

type colima struct{}

func (colima) Name() string { return "colima" }

// ListArgv lists every profile; the listing is read-only and filtered by name here, because a
// per-profile listing of one that does not exist is an error rather than an empty answer.
func (colima) ListArgv(Config) []string { return []string{"colima", "list", "--json"} }

func (colima) ParseState(out, name string) (State, error) {
	s, _, err := jsonLinesState(out, name)

	return s, err
}

func (colima) CreateArgv(Config) []string { return nil }

// StartArgv uses the docker runtime because the firecracker provider builds each rootfs through a
// docker engine (`docker export`), and no port forwarder, for the same reason lima gets its ignore
// rules. The docker runtime is also what makes colima repoint the global docker context on start,
// which is why every colima start runs under Manager.guardDockerContext.
func (colima) StartArgv(c Config) []string {
	return []string{
		"colima", "start", "--profile", c.Name,
		"--vm-type", "vz", "--nested-virtualization",
		"--cpus", strconv.Itoa(c.CPUs), "--memory", gib(c.MemoryGiB), "--disk", strconv.Itoa(c.DiskGiB),
		"--runtime", "docker", "--port-forwarder", "none",
	}
}

func (colima) StopArgv(c Config) []string { return []string{"colima", "stop", "--profile", c.Name} }

func (colima) RemoveArgv(c Config) []string {
	return []string{"colima", "delete", "--profile", c.Name, "--force"}
}

func (colima) SSHConfigArgv(c Config) []string {
	return []string{"colima", "ssh-config", "--profile", c.Name}
}

func (colima) ShellArgv(_ Config, t transport, dir string, tty, root bool, argv []string) []string {
	return sshShell(t, dir, tty, root, argv)
}

func (colima) SwitchesDockerContext() bool { return true }
func (colima) NativeForwarding() bool      { return false }

// --- wsl --------------------------------------------------------------------------------------

// wsl drives a WSL2 distro. Unverified end to end: no Windows host was available, so this is
// command construction and parsing, tested, and nothing more.
type wsl struct{}

func (wsl) Name() string { return "wsl" }

func (wsl) ListArgv(Config) []string { return []string{"wsl.exe", "--list", "--verbose"} }

// ParseState reads `wsl --list --verbose`, which wsl.exe writes in UTF-16LE.
func (wsl) ParseState(out, name string) (State, error) {
	sc := bufio.NewScanner(strings.NewReader(decodeUTF16(out)))
	for sc.Scan() {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "*"))
		if len(f) < 2 || f[0] != name {
			continue
		}

		switch f[1] {
		case "Running":
			return Running, nil
		case "Stopped":
			return Stopped, nil
		default:
			return "", fmt.Errorf("WSL distro %s is %q; wait for it to settle, or `sbx fc vm rm`", name, f[1])
		}
	}

	return Absent, nil
}

func decodeUTF16(s string) string {
	if !strings.ContainsRune(s, 0) || len(s)%2 != 0 {
		return s
	}

	u := make([]uint16, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		u = append(u, uint16(s[i])|uint16(s[i+1])<<8)
	}

	return strings.TrimPrefix(string(utf16.Decode(u)), "\ufeff")
}

// CreateArgv installs a named distro without launching it, so no interactive user setup runs:
// sbx uses it as root through `-u root` and never needs one.
func (wsl) CreateArgv(c Config) []string {
	return []string{"wsl.exe", "--install", "Ubuntu-24.04", "--name", c.Name, "--no-launch"}
}

// StartArgv runs a no-op; a WSL distro starts on its first command.
func (wsl) StartArgv(c Config) []string {
	return []string{"wsl.exe", "-d", c.Name, "-u", "root", "--exec", "true"}
}

func (wsl) StopArgv(c Config) []string   { return []string{"wsl.exe", "--terminate", c.Name} }
func (wsl) RemoveArgv(c Config) []string { return []string{"wsl.exe", "--unregister", c.Name} }

func (wsl) SSHConfigArgv(Config) []string { return nil }

// ShellArgv uses --exec so no shell re-splits the argv, and -u root because the distro was
// never given a user.
func (wsl) ShellArgv(c Config, _ transport, dir string, _, _ bool, argv []string) []string {
	out := []string{"wsl.exe", "-d", c.Name, "-u", "root"}
	if dir != "" {
		out = append(out, "--cd", dir)
	}

	return append(append(out, "--exec"), argv...)
}

func (wsl) SwitchesDockerContext() bool { return false }
func (wsl) NativeForwarding() bool      { return true }

var errNotRunning = errors.New("the helper VM is not running")
