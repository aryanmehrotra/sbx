package fchost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
)

// fakeRunner records every command and answers from a script keyed by argv prefix. Nothing it
// is handed ever runs, which is the point: these tests prove what sbx WOULD run against lima,
// colima and wsl without touching a VM.
type fakeRunner struct {
	mu      sync.Mutex
	calls   [][]string
	stdin   []string
	answers []answer
}

type answer struct {
	prefix   string
	contains string // when set, matched anywhere in the line instead of prefix
	out      string
	err      error
	times    int // 0 = forever; otherwise used up after this many matches
}

func (f *fakeRunner) on(prefix, out string, err error) *fakeRunner {
	f.answers = append(f.answers, answer{prefix: prefix, out: out, err: err})

	return f
}

func (f *fakeRunner) onContains(sub, out string, err error) *fakeRunner {
	f.answers = append(f.answers, answer{contains: sub, out: out, err: err})

	return f
}

func (f *fakeRunner) once(prefix, out string) *fakeRunner {
	f.answers = append(f.answers, answer{prefix: prefix, out: out, times: 1})

	return f
}

func (f *fakeRunner) answer(c Cmd) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, c.Argv)

	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		f.stdin = append(f.stdin, string(b))
	}

	line := strings.Join(c.Argv, " ")

	for i := range f.answers {
		a := &f.answers[i]
		switch {
		case a.times < 0:
			continue
		case a.contains != "":
			if !strings.Contains(line, a.contains) {
				continue
			}
		case !strings.HasPrefix(line, a.prefix):
			continue
		}

		if a.times > 0 {
			a.times--
			if a.times == 0 {
				a.times = -1
			}
		}

		return a.out, a.err
	}

	return "", nil
}

func (f *fakeRunner) Output(_ context.Context, c Cmd) (string, error) { return f.answer(c) }

func (f *fakeRunner) Run(_ context.Context, c Cmd) error {
	out, err := f.answer(c)
	if c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, out)
	}

	return err
}

func (f *fakeRunner) Start(_ context.Context, c Cmd) (func() error, error) {
	_, err := f.answer(c)

	return func() error { return nil }, err
}

func (f *fakeRunner) lines() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}

	return out
}

var testCfg = Config{Name: "sbx-fc", CPUs: 2, MemoryGiB: 2, DiskGiB: 20}

func TestLimaCommands(t *testing.T) {
	d := lima{}

	create := strings.Join(d.CreateArgv(testCfg), " ")
	for _, w := range []string{
		"limactl create --tty=false --name sbx-fc",
		"--vm-type vz", "--nested-virt", "--cpus 2", "--memory 2", "--disk 20",
		"--containerd none", "template:ubuntu-24.04",
		// Every guest port ignored: the only way to a sandbox from the host is the connect
		// tunnel, so a port lima forwarded on its own would race it for the same number.
		`"ignore":true`, `"guestPortRange":[1,65535]`,
	} {
		if !strings.Contains(create, w) {
			t.Errorf("create %q lacks %q", create, w)
		}
	}

	for got, want := range map[string]string{
		strings.Join(d.StartArgv(testCfg), " "):  "limactl start --tty=false sbx-fc",
		strings.Join(d.StopArgv(testCfg), " "):   "limactl stop --tty=false sbx-fc",
		strings.Join(d.RemoveArgv(testCfg), " "): "limactl delete --tty=false --force sbx-fc",
		strings.Join(d.ListArgv(testCfg), " "):   "limactl list --json",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestColimaCommandsNeverLeaveTheirProfile(t *testing.T) {
	d := colima{}

	start := strings.Join(d.StartArgv(testCfg), " ")
	for _, w := range []string{
		"colima start --profile sbx-fc", "--vm-type vz", "--nested-virtualization",
		"--cpus 2", "--memory 2", "--disk 20", "--runtime docker", "--port-forwarder none", "--activate=false",
	} {
		if !strings.Contains(start, w) {
			t.Errorf("start %q lacks %q", start, w)
		}
	}

	// Every mutating colima command names the profile explicitly. colima's default profile is
	// "default", and that is somebody's docker.
	for _, argv := range [][]string{d.StartArgv(testCfg), d.StopArgv(testCfg), d.RemoveArgv(testCfg), d.SSHConfigArgv(testCfg)} {
		i := slices.Index(argv, "--profile")
		if i < 0 || i+1 >= len(argv) || argv[i+1] != "sbx-fc" {
			t.Errorf("%q does not pin --profile sbx-fc", argv)
		}
	}

	if d.CreateArgv(testCfg) != nil {
		t.Error("colima creates on start; a separate create would be a second, different command")
	}
}

func TestWSLCommands(t *testing.T) {
	d := wsl{}

	for got, want := range map[string]string{
		strings.Join(d.CreateArgv(testCfg), " "): "wsl.exe --install Ubuntu-24.04 --name sbx-fc --no-launch",
		strings.Join(d.StartArgv(testCfg), " "):  "wsl.exe -d sbx-fc -u root --exec true",
		strings.Join(d.StopArgv(testCfg), " "):   "wsl.exe --terminate sbx-fc",
		strings.Join(d.RemoveArgv(testCfg), " "): "wsl.exe --unregister sbx-fc",
		strings.Join(d.ListArgv(testCfg), " "):   "wsl.exe --list --verbose",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}

	sh := strings.Join(d.ShellArgv(testCfg, transport{}, `C:\src\app`, false, true, []string{"sbx", "list"}), " ")
	if sh != `wsl.exe -d sbx-fc -u root --cd C:\src\app --exec sbx list` {
		t.Errorf("shell = %q", sh)
	}
}

func utf16le(s string) string {
	var b strings.Builder

	for _, r := range utf16.Encode([]rune(s)) {
		b.WriteByte(byte(r))
		b.WriteByte(byte(r >> 8))
	}

	return b.String()
}

func TestParseState(t *testing.T) {
	limaOut := `{"name":"other","status":"Running"}
{"name":"sbx-fc","status":"Stopped","sshConfigFile":"/Users/me/.lima/sbx-fc/ssh.config"}
`
	colimaOut := `{"name":"default","status":"Running","runtime":"docker"}
{"name":"sbx-fc","status":"Running","arch":"aarch64"}
`
	wslOut := utf16le("  NAME            STATE           VERSION\r\n* Ubuntu          Running         2\r\n  sbx-fc          Stopped         2\r\n")

	cases := []struct {
		d    Driver
		out  string
		name string
		want State
	}{
		{lima{}, limaOut, "sbx-fc", Stopped},
		{lima{}, limaOut, "other", Running},
		{lima{}, "", "sbx-fc", Absent},
		{colima{}, colimaOut, "sbx-fc", Running},
		{colima{}, colimaOut, "sbx-x", Absent},
		{wsl{}, wslOut, "sbx-fc", Stopped},
		{wsl{}, wslOut, "Ubuntu", Running},
		{wsl{}, utf16le("Windows Subsystem for Linux has no installed distributions.\r\n"), "sbx-fc", Absent},
	}

	for _, c := range cases {
		got, err := c.d.ParseState(c.out, c.name)
		if err != nil || got != c.want {
			t.Errorf("%s %s = %s, %v; want %s", c.d.Name(), c.name, got, err, c.want)
		}
	}

	if _, err := (lima{}).ParseState(`{"name":"sbx-fc","status":"Broken"}`, "sbx-fc"); err == nil {
		t.Error("a Broken instance must be an error, not a state to start from")
	}
}

func TestConfigRefusesNamesThatAreNotSbxs(t *testing.T) {
	for _, bad := range []string{"default", "osb", "fc", "colima", "sbx-", "sbx-FC", "sbx fc", "sbx-fc;rm"} {
		c := testCfg
		c.Name = bad

		if err := c.Validate(); err == nil {
			t.Errorf("%q accepted: sbx must only ever manage a VM it named", bad)
		}
	}

	for _, bad := range []Config{
		{Name: "sbx-fc", CPUs: 0, MemoryGiB: 2, DiskGiB: 20},
		{Name: "sbx-fc", CPUs: 2, MemoryGiB: 0.5, DiskGiB: 20},
		{Name: "sbx-fc", CPUs: 2, MemoryGiB: 2, DiskGiB: 5},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}

	if err := testCfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{"SBX_FC_VM_CPUS": "4", "SBX_FC_VM_MEMORY": "3.5", "SBX_FC_VM_DISK": "40"}

	c, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}

	if c != (Config{Name: "sbx-fc", CPUs: 4, MemoryGiB: 3.5, DiskGiB: 40}) {
		t.Fatalf("%+v", c)
	}

	env["SBX_FC_VM_CPUS"] = "lots"
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "SBX_FC_VM_CPUS") {
		t.Fatalf("a bad value must name its variable: %v", err)
	}
}

// The script handed to ssh is a shell string, so argv quoting is the one thing that must be
// right. Proven by running it through a real sh rather than by reading it.
func TestSSHShellQuotesArgvExactly(t *testing.T) {
	argv := []string{"printf", "%s|", "plain", "two words", "it's", `"dq"`, "$HOME", "a;b", "`x`", ""}

	full := lima{}.ShellArgv(testCfg, transport{sshConfig: "/cfg", sshHost: "lima-sbx-fc"}, "/no/such/dir", false, false, argv)

	want := []string{"ssh", "-F", "/cfg", "-o", "LogLevel=ERROR", "-T", "lima-sbx-fc"}
	if !slices.Equal(full[:len(want)], want) {
		t.Fatalf("ssh prefix = %q", full[:len(want)])
	}

	script := full[len(full)-1]

	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}

	if got := string(out); got != `plain|two words|it's|"dq"|$HOME|a;b|`+"`x`"+`||` {
		t.Fatalf("argv did not survive: %q", got)
	}

	root := lima{}.ShellArgv(testCfg, transport{sshConfig: "/cfg", sshHost: "lima-sbx-fc"}, "/x", true, true, []string{"sbx", "list"})
	if !slices.Contains(root, "-t") || !strings.Contains(root[len(root)-1], "exec sudo -H 'sbx' 'list'") {
		t.Fatalf("root+tty shell = %q", root)
	}
}

func TestColimaSwitchesTheDockerContextBackEvenOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			var startErr error
			if fail {
				startErr = errors.New("exit status 1")
			}

			r := (&fakeRunner{}).
				once("colima list", "").
				once("docker context show", "osb\n").
				on("colima start", "", startErr).
				on("docker context show", "colima-sbx-fc\n", nil)

			m := &Manager{Driver: colima{}, Config: testCfg, Run: r, Out: io.Discard, StateDir: t.TempDir()}

			err := m.Start(context.Background())
			if fail != (err != nil) {
				t.Fatalf("err = %v", err)
			}

			if !slices.Contains(r.lines(), "docker context use osb") {
				t.Fatalf("context not restored: %q", r.lines())
			}
		})
	}
}

func TestColimaLeavesAnUnchangedContextAlone(t *testing.T) {
	r := (&fakeRunner{}).once("colima list", "").on("docker context show", "default\n", nil)
	m := &Manager{Driver: colima{}, Config: testCfg, Run: r, Out: io.Discard, StateDir: t.TempDir()}

	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, l := range r.lines() {
		if strings.HasPrefix(l, "docker context use") {
			t.Fatalf("switched a context nobody changed: %q", l)
		}
	}
}

func TestStartCreatesOnlyWhatIsAbsent(t *testing.T) {
	cases := []struct {
		list string
		want []string
		not  []string
	}{
		{"", []string{"limactl create", "limactl start"}, nil},
		{`{"name":"sbx-fc","status":"Stopped"}`, []string{"limactl start"}, []string{"limactl create"}},
		{`{"name":"sbx-fc","status":"Running"}`, nil, []string{"limactl create", "limactl start"}},
	}

	for _, c := range cases {
		r := (&fakeRunner{}).on("limactl list", c.list, nil)
		m := &Manager{Driver: lima{}, Config: testCfg, Run: r, Out: io.Discard, StateDir: t.TempDir()}

		if err := m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}

		got := strings.Join(r.lines(), "\n")

		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("list %q: ran\n%s\nwithout %q", c.list, got, w)
			}
		}

		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("list %q: ran %q", c.list, n)
			}
		}
	}
}

func TestStopAndRemoveOfAnAbsentVMDoNothing(t *testing.T) {
	r := (&fakeRunner{}).on("limactl list", "", nil)
	m := &Manager{Driver: lima{}, Config: testCfg, Run: r, Out: io.Discard, StateDir: t.TempDir()}

	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := m.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, l := range r.lines() {
		if !strings.HasPrefix(l, "limactl list") {
			t.Fatalf("ran %q against a VM that does not exist", l)
		}
	}
}
