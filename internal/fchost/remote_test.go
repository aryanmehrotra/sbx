package fchost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// loopRunner is the helper VM with the ssh hop taken out: lima says the VM is running, and a
// shell command that is `sbx fc call ...` runs Call against prov in this process, so every
// request and answer crosses the same encoding it would cross between the Mac and the VM.
type loopRunner struct {
	prov  provider.Provider
	mu    sync.Mutex
	shell [][]string
}

var callRE = regexp.MustCompile(`'call' '([a-z]+)' '([A-Za-z0-9_-]+)'`)

func (l *loopRunner) Output(_ context.Context, c Cmd) (string, error) {
	if c.Argv[0] == "limactl" && slices.Contains(c.Argv, "list") {
		return runningLima, nil
	}

	return "", nil
}

func (l *loopRunner) Run(ctx context.Context, c Cmd) error {
	l.mu.Lock()
	l.shell = append(l.shell, c.Argv)
	l.mu.Unlock()

	// The ssh driver quotes the in-VM argv into one shell string: 'call' 'method' 'request'.
	m := callRE.FindStringSubmatch(strings.Join(c.Argv, " "))
	if m == nil {
		return errors.New("not a call: " + strings.Join(c.Argv, " "))
	}

	stdin := c.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}

	stdout, stderr := c.Stdout, c.Stderr
	if stdout == nil {
		stdout = io.Discard
	}

	if stderr == nil {
		stderr = io.Discard
	}

	if err := Call(ctx, l.prov, m[1:], stdin, stdout, stderr); err != nil {
		_, _ = io.WriteString(stderr, err.Error())
		return errors.New("exit status 1")
	}

	return nil
}

func (l *loopRunner) Start(context.Context, Cmd) (func() error, error) {
	return nil, errors.New("not used")
}

// recProvider is the in-VM firecracker provider, reduced to what each call received.
type recProvider struct {
	provider.Provider

	created   spec.Service
	createdAt [3]int
	iso       provider.Isolation
	eps       []provider.Endpoint
	copied    [2]string
	copyData  string
	execArgv  []string
}

func (p *recProvider) Name() string { return "firecracker" }

func (p *recProvider) Create(_ context.Context, sandbox string, slot, ordinal int, service string,
	svc spec.Service, eps []provider.Endpoint, specDir string, iso provider.Isolation,
) error {
	if sandbox != "demo" || service != "cache" || specDir != "" {
		return errors.New("wrong create target")
	}

	p.created, p.createdAt, p.eps, p.iso = svc, [3]int{slot, ordinal}, eps, iso

	return nil
}

func (p *recProvider) Start(_ context.Context, ref string) error {
	if ref != "sbx-demo-cache" {
		return errors.New("no such ref " + ref)
	}

	return nil
}

func (p *recProvider) Healthy(context.Context, string) (bool, bool) { return true, false }

func (p *recProvider) List(context.Context, string) ([]provider.Unit, error) {
	return []provider.Unit{{Sandbox: "demo", Service: "cache", Ref: "sbx-demo-cache", Running: true,
		Upstream: []provider.Endpoint{{Host: "10.231.0.2", Port: 6379}}}}, nil
}

func (p *recProvider) Exec(_ context.Context, _ string, argv []string) (string, error) {
	p.execArgv = argv
	if argv[0] == "false" {
		return "", &provider.ExitError{Code: 3, Output: "nope"}
	}

	return "PONG", nil
}

func (p *recProvider) Copy(_ context.Context, _ string, src, dst string) error {
	p.copied = [2]string{src, dst}

	if strings.HasPrefix(dst, ":") {
		b, err := os.ReadFile(src)
		p.copyData = string(b)

		return err
	}

	return os.WriteFile(dst, []byte("from the vm"), 0o600)
}

func (p *recProvider) Logs(_ context.Context, _ string, lines int, _ bool, w io.Writer) error {
	_, err := io.WriteString(w, strings.Repeat("line\n", lines))
	return err
}

func (p *recProvider) Endpoints(_, _ string, slot, start int, ports []int) []provider.Endpoint {
	return []provider.Endpoint{{Host: "127.0.0.1", Port: 20000 + slot*10 + start + len(ports)}}
}

func (p *recProvider) AllocSlot(context.Context, string) (int, error) { return 4, nil }

func remote(t *testing.T) (*Remote, *recProvider, *loopRunner) {
	t.Helper()

	rp := &recProvider{}
	lr := &loopRunner{prov: rp}

	return &Remote{M: current(t, &Manager{Driver: lima{}, Config: testCfg, Run: lr, Out: io.Discard, StateDir: t.TempDir()})}, rp, lr
}

func TestHelperVMProviderIsWired(t *testing.T) {
	if provider.HelperVMProvider == nil {
		t.Fatal("fchost must install provider.HelperVMProvider, or --provider firecracker on a Mac says the layer is missing")
	}
}

func TestRemoteRunsEveryMethodInTheVM(t *testing.T) {
	r, rp, lr := remote(t)
	ctx := t.Context()

	svc := spec.Service{Image: "redis:7-alpine", Ports: []int{6379}, Env: map[string]string{"A": "1"}, Memory: "128m"}
	eps := []provider.Endpoint{{Host: "127.0.0.1", Port: 20060}}

	if err := r.Create(ctx, "demo", 2, 1, "cache", svc, eps, "/Users/me/spec", provider.IsolationFirecracker); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if rp.created.Image != svc.Image || rp.created.Env["A"] != "1" || rp.created.Memory != "128m" ||
		rp.createdAt != [3]int{2, 1} || !slices.Equal(rp.eps, eps) || rp.iso != provider.IsolationFirecracker {
		t.Fatalf("the VM received %+v at %v, eps %v, iso %s", rp.created, rp.createdAt, rp.eps, rp.iso)
	}

	if err := r.Start(ctx, "sbx-demo-cache"); err != nil {
		t.Fatal(err)
	}

	if err := r.Start(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "no such ref nope") {
		t.Fatalf("an in-VM error must come back with its text: %v", err)
	}

	if s, d := r.Healthy(ctx, "x"); !s || d {
		t.Fatalf("Healthy = %v %v", s, d)
	}

	units, err := r.List(ctx, "")
	if err != nil || len(units) != 1 || units[0].Upstream[0].Port != 6379 || !units[0].Running {
		t.Fatalf("List = %+v, %v", units, err)
	}

	if out, err := r.Exec(ctx, "sbx-demo-cache", []string{"redis-cli", "ping"}); err != nil || out != "PONG" {
		t.Fatalf("Exec = %q, %v", out, err)
	}

	_, err = r.Exec(ctx, "sbx-demo-cache", []string{"false"})

	var ee *provider.ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || ee.Output != "nope" {
		t.Fatalf("a non-zero exit must survive the hop: %v", err)
	}

	if slot, err := r.AllocSlot(ctx, "demo"); err != nil || slot != 4 {
		t.Fatalf("AllocSlot = %d, %v", slot, err)
	}

	if eps := r.Endpoints("demo", "cache", 3, 1, []int{1, 2}); len(eps) != 1 || eps[0].Port != 20033 {
		t.Fatalf("Endpoints = %v", eps)
	}

	var logs bytes.Buffer
	if err := r.Logs(ctx, "sbx-demo-cache", 3, false, &logs); err != nil || logs.String() != "line\nline\nline\n" {
		t.Fatalf("Logs = %q, %v", logs.String(), err)
	}

	// Every call is the in-VM sbx, as root, with the provider set in its environment.
	for _, sh := range lr.shell {
		line := strings.Join(sh, " ")
		if !strings.Contains(line, "exec sudo -H 'env' 'SBX_PROVIDER_KIND=firecracker' '/usr/local/bin/sbx' 'fc' 'call'") {
			t.Fatalf("not an in-VM call: %s", line)
		}
	}
}

func TestRemoteCopyCrossesBothBoundaries(t *testing.T) {
	r, rp, _ := remote(t)
	ctx := t.Context()
	dir := t.TempDir()

	src := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(src, []byte("from the mac"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := r.Copy(ctx, "sbx-demo-cache", src, ":/data/notes.txt"); err != nil {
		t.Fatalf("copy in: %v", err)
	}

	if rp.copyData != "from the mac" || rp.copied[1] != ":/data/notes.txt" || filepath.Base(rp.copied[0]) != "notes.txt" {
		t.Fatalf("the VM copied %v with %q", rp.copied, rp.copyData)
	}

	if err := r.Copy(ctx, "sbx-demo-cache", ":/data/out.txt", dir); err != nil {
		t.Fatalf("copy out: %v", err)
	}

	if b, _ := os.ReadFile(filepath.Join(dir, "out.txt")); string(b) != "from the vm" {
		t.Fatalf("copied out %q", b)
	}

	if err := r.Copy(ctx, "sbx-demo-cache", dir, ":/data"); err == nil {
		t.Fatal("a directory was accepted")
	}
}
