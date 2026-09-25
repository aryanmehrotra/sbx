package provider

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/fc/fcfake"
	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// The firecracker provider's state machine, against fcfake: every VMM call it makes, in order,
// and what it leaves on disk - without KVM, on any OS. What this cannot show (a real guest
// booting, a real restore answering) is the SBX_FC_E2E test's job.

type fakeLauncher struct {
	mu       sync.Mutex
	servers  map[string]*fcfake.Server
	launches int
	kills    int
}

func (l *fakeLauncher) Launch(_ context.Context, s fc.LaunchSpec) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if old := l.servers[s.Dir]; old != nil {
		_ = old.Close()
	}

	srv, err := fcfake.Start(filepath.Join(s.Dir, fc.APISockName))
	if err != nil {
		return 0, err
	}

	l.servers[s.Dir] = srv
	l.launches++

	return 1000 + l.launches, nil
}

func (l *fakeLauncher) Kill(_ context.Context, dir string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if srv := l.servers[dir]; srv != nil {
		_ = srv.Close()
		delete(l.servers, dir)
		l.kills++
	}

	return nil
}

func (l *fakeLauncher) server(dir string) *fcfake.Server {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.servers[dir]
}

type fakeNet struct {
	mu      sync.Mutex
	taps    map[string]bool
	bridges map[int]bool
}

func (n *fakeNet) EnsureTap(_ context.Context, a fc.Addr) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.taps[a.Tap()], n.bridges[a.Slot] = true, true

	return nil
}

func (n *fakeNet) RemoveTap(_ context.Context, a fc.Addr) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	delete(n.taps, a.Tap())

	return nil
}

func (n *fakeNet) RemoveBridge(_ context.Context, slot int) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	delete(n.bridges, slot)

	return nil
}

type touchExt4 struct{}

func (touchExt4) TakesTar(context.Context) bool { return true }

func (touchExt4) Build(_ context.Context, _, img, _ string) error {
	return os.WriteFile(img, []byte("ext4"), 0o600)
}

type tarEngine struct{}

func (tarEngine) Inspect(context.Context, string) (fc.ImageConfig, error) {
	return fc.ImageConfig{ID: "sha256:" + strings.Repeat("ab", 32), Cmd: []string{"redis-server"}, Env: []string{"PATH=/usr/bin"}}, nil
}

func (tarEngine) Pull(context.Context, string) error { return nil }

func (tarEngine) Export(_ context.Context, _ string, w io.Writer) error {
	tw := tar.NewWriter(w)
	_ = tw.WriteHeader(&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755})

	return tw.Close()
}

func (tarEngine) CopyOut(context.Context, string, string, string) error { return nil }

// fakeGuest records Seal and Rekey, and checks the VM's state at the moment each happens:
// Seal must reach a RUNNING VM (before the pause), Rekey a resumed one (before Start returns).
type fakeGuest struct {
	mu         sync.Mutex
	available  bool
	seals      []string // VM state when sealed
	rekeys     []fc.Rekey
	rekeyState []string
	failSeal   error
	failRekey  error
	launcher   *fakeLauncher
	onSeal     func(secret string)
}

func (g *fakeGuest) Available() bool { return g.available }

func (g *fakeGuest) Dial(context.Context, fc.GuestVM, int) (net.Conn, error) {
	return nil, errors.New("fake")
}

func (g *fakeGuest) Seal(_ context.Context, vm fc.GuestVM, secret string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if secret == "" {
		return errors.New("sealed without the control secret")
	}

	g.seals = append(g.seals, g.launcher.server(vm.Dir).State())

	if g.onSeal != nil {
		g.onSeal(secret)
	}

	return g.failSeal
}

func (g *fakeGuest) Rekey(_ context.Context, vm fc.GuestVM, k fc.Rekey) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.rekeys = append(g.rekeys, k)
	g.rekeyState = append(g.rekeyState, g.launcher.server(vm.Dir).State())

	return g.failRekey
}

type rig struct {
	p     *fcProvider
	l     *fakeLauncher
	n     *fakeNet
	g     *fakeGuest
	root  string
	ctx   context.Context
	merge int
}

func newRig(t *testing.T) *rig {
	t.Helper()

	// Short: the API socket lives under here and macOS caps a socket path at 104 bytes.
	root, err := os.MkdirTemp("", "fcp")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { os.RemoveAll(root) })

	files := map[string]string{}
	for _, f := range []string{"firecracker", "vmlinux", "sbx-linux"} {
		files[f] = filepath.Join(root, f)
		if err := os.WriteFile(files[f], []byte(f), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	r := &rig{root: root, ctx: context.Background()}
	r.l = &fakeLauncher{servers: map[string]*fcfake.Server{}}
	r.n = &fakeNet{taps: map[string]bool{}, bridges: map[int]bool{}}
	r.g = &fakeGuest{available: true, launcher: r.l}

	t.Cleanup(func() {
		for d := range r.l.servers {
			_ = r.l.Kill(context.Background(), d)
		}
	})

	arts := &fc.ArtifactCache{Dir: filepath.Join(root, "artifacts"), Getenv: func(k string) string {
		return map[string]string{"SBX_FC_BINARY": files["firecracker"], "SBX_FC_KERNEL": files["vmlinux"]}[k]
	}}

	r.p = &fcProvider{
		root: root, arch: "arm64", arts: arts, ext4: touchExt4{}, net: r.n, launch: r.l, guest: r.g,
		rootfs: &fc.RootfsBuilder{Dir: filepath.Join(root, "rootfs"), Engine: tarEngine{}, Ext4: touchExt4{}, Headroom: 1 << 20},
		agent:  func(context.Context, string) (string, error) { return files["sbx-linux"], nil },
		ready:  func(context.Context, *fcVM, string) error { return nil },
		merge: func(diff, base string) error {
			r.merge++
			return nil
		},
		bootTimeout: time.Second,
		portsFree:   func(int) bool { return true },
		locks:       map[string]*sync.Mutex{},
	}

	return r
}

func (r *rig) create(t *testing.T, sandbox string, svc spec.Service) string {
	t.Helper()

	slot, err := r.p.AllocSlot(r.ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}

	eps := r.p.Endpoints(sandbox, "cache", slot, 0, svc.Ports)
	if err := r.p.Create(r.ctx, sandbox, slot, 0, "cache", svc, eps, "", IsolationContainer); err != nil {
		t.Fatal(err)
	}

	return containerName(sandbox, "cache")
}

func (r *rig) vm(t *testing.T, ref string) *fcVM {
	t.Helper()

	vm, err := r.p.load(ref)
	if err != nil {
		t.Fatal(err)
	}

	return vm
}

func pathsOf(calls []fcfake.Call) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Method+" "+c.Path)
	}

	return out
}

var redis = spec.Service{Image: "redis:7-alpine", Ports: []int{6379}, Env: map[string]string{"A": "1"}}

func TestCreateLeavesTheVMAsleepWithAFullSnapshot(t *testing.T) {
	r := newRig(t)

	r.g.available = false // no Seal on create without a guest; covered separately

	ref := r.create(t, "t1", redis)
	dir := r.p.dir(ref)

	vm := r.vm(t, ref)
	if !vm.SnapshotValid || vm.Restored || vm.Clone == "" || vm.AccessToken == "" || vm.ControlSecret == "" {
		t.Fatalf("record after create = %+v", vm)
	}

	for _, f := range []string{fc.StateName, fc.MemName, fc.RootfsName, "agent.ext4"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s missing after create: %v", f, err)
		}
	}

	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("VM dir mode %v; it holds the access token", st.Mode().Perm())
	}

	if r.l.launches != 1 || r.l.kills != 1 || r.l.server(dir) != nil {
		t.Fatalf("launches=%d kills=%d: create must leave no process running", r.l.launches, r.l.kills)
	}

	units, err := r.p.List(r.ctx, "t1")
	if err != nil || len(units) != 1 {
		t.Fatalf("List = %v, %v", units, err)
	}

	u := units[0]
	if u.Running || u.Paused || u.Ref != ref || u.Upstream[0].Host != "10.231.0.2" || u.Upstream[0].Port != 6379 ||
		u.Client[0].Port != publicBase || u.Listen[0] != publicBase {
		t.Fatalf("unit = %+v", u)
	}

	if !r.n.taps["sbxfc0-0"] {
		t.Fatalf("taps = %v", r.n.taps)
	}
}

func TestCreateBootsWithTheRightConfig(t *testing.T) {
	r := newRig(t)
	r.g.available = false

	var seen []string

	// Snapshot the boot calls at readiness time, while the fake still holds them.
	r.p.ready = func(_ context.Context, vm *fcVM, dir string) error {
		srv := r.l.server(dir)
		seen = pathsOf(srv.Calls())

		boot := srv.Calls()[0].Body
		if args, _ := boot["boot_args"].(string); !strings.Contains(args, "init=/sbx") ||
			!strings.Contains(args, "-- fc-init") || !strings.Contains(args, "ip=10.231.0.2::10.231.0.1") {
			t.Errorf("boot_args = %q", args)
		}

		for _, c := range srv.Calls() {
			switch c.Path {
			case "/drives/agent":
				if c.Body["is_root_device"] != true || c.Body["is_read_only"] != true {
					t.Errorf("agent drive = %v", c.Body)
				}
			case "/drives/rootfs":
				if c.Body["is_root_device"] != false || c.Body["is_read_only"] != false {
					t.Errorf("rootfs drive = %v", c.Body)
				}
			case "/machine-config":
				if c.Body["track_dirty_pages"] != true || c.Body["vcpu_count"] != float64(2) || c.Body["mem_size_mib"] != float64(512) {
					t.Errorf("machine-config = %v", c.Body)
				}
			}
		}

		return nil
	}

	svc := redis
	svc.CPU, svc.Memory = "1.5", "512m"
	r.create(t, "t2", svc)

	want := []string{
		"PUT /boot-source", "PUT /drives/agent", "PUT /drives/rootfs", "PUT /machine-config",
		"PUT /network-interfaces/eth0", "PUT /vsock", "PUT /entropy", "PUT /actions",
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("boot calls:\n got %v\nwant %v", seen, want)
	}
}

func TestWakeRestoresAndRekeysAndSleepTakesADiff(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "t3", redis)
	dir := r.p.dir(ref)

	if len(r.g.seals) != 1 || r.g.seals[0] != "Running" {
		t.Fatalf("create must seal a running VM before its snapshot: %v", r.g.seals)
	}

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	srv := r.l.server(dir)
	calls := srv.Calls()

	if got := pathsOf(calls); !slices.Equal(got, []string{"PUT /snapshot/load"}) {
		t.Fatalf("a wake must be a load, not a boot: %v", got)
	}

	load := calls[0].Body
	if load["resume_vm"] != true || load["track_dirty_pages"] != true {
		t.Fatalf("load = %v", load)
	}

	if ov := load["vsock_override"].(map[string]any); ov["uds_path"] != filepath.Join(dir, fc.VsockName) {
		t.Fatalf("vsock_override = %v", ov)
	}

	if ovs := load["network_overrides"].([]any); ovs[0].(map[string]any)["host_dev_name"] != "sbxfc0-0" {
		t.Fatalf("network_overrides = %v", ovs)
	}

	// Re-keyed before Start returned, on a resumed VM, with the next generation, the access
	// token every process agrees on, authorised by the secret the snapshot holds (the boot one)
	// and rotating it: execd refuses a re-key that keeps the secret every clone shares.
	vm := r.vm(t, ref)
	if len(r.g.rekeys) != 1 || r.g.rekeyState[0] != "Running" || r.g.rekeys[0].Generation != 1 ||
		r.g.rekeys[0].AccessToken != vm.AccessToken || r.g.rekeys[0].Secret != vm.ControlSecret ||
		r.g.rekeys[0].ControlSecret == vm.ControlSecret || len(r.g.rekeys[0].ControlSecret) < 32 ||
		vm.LiveSecret != r.g.rekeys[0].ControlSecret {
		t.Fatalf("rekeys = %+v in states %v, live secret %q", r.g.rekeys, r.g.rekeyState, vm.LiveSecret)
	}

	if vm.SnapshotValid || !vm.Restored {
		t.Fatalf("a running restored VM must hold an INVALID snapshot: %+v", vm)
	}

	if units, _ := r.p.List(r.ctx, "t3"); !units[0].Running {
		t.Fatal("List does not see it running")
	}

	// Start on a running VM is a no-op, not a second load.
	if err := r.p.Start(r.ctx, ref); err != nil || r.l.launches != 2 {
		t.Fatalf("second Start: %v, launches %d", err, r.l.launches)
	}

	r.p.merge = func(diff, base string) error {
		r.merge++

		if b, _ := os.ReadFile(diff); string(b) != "mem:Diff" {
			t.Errorf("merged %q, want the diff file", b)
		}

		return nil
	}

	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if r.merge != 1 {
		t.Fatalf("merges = %d; a restored VM sleeps as a Diff folded into its base", r.merge)
	}

	if len(r.g.seals) != 2 || r.g.seals[1] != "Running" {
		t.Fatalf("seals = %v", r.g.seals)
	}

	vm = r.vm(t, ref)
	if !vm.SnapshotValid || vm.Restored {
		t.Fatalf("after sleep = %+v", vm)
	}

	if _, err := os.Stat(filepath.Join(dir, fc.DiffMemName)); err == nil {
		t.Fatal("diff.mem left behind after the merge")
	}

	if r.l.server(dir) != nil {
		t.Fatal("Stop left the VMM running")
	}

	// Stop on a sleeping VM is a no-op.
	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}
}

func TestAVMThatDiedAwakeColdBoots(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "t4", redis)
	dir := r.p.dir(ref)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	// The VMM dies with no snapshot: a crash, a host reboot, a workload that exited.
	_ = r.l.Kill(r.ctx, dir)

	if units, _ := r.p.List(r.ctx, "t4"); units[0].Running {
		t.Fatal("a dead VMM listed as running")
	}

	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatalf("stopping a dead VM: %v", err)
	}

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	got := pathsOf(r.l.server(dir).Calls())
	if slices.Contains(got, "PUT /snapshot/load") || !slices.Contains(got, "PUT /actions") {
		t.Fatalf("stale memory restored over a newer disk: %v", got)
	}

	// And its next sleep is Full, since nothing was loaded to diff against.
	merges := r.merge
	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if r.merge != merges {
		t.Fatal("a cold-booted VM slept as a Diff")
	}

	if !r.vm(t, ref).SnapshotValid {
		t.Fatal("snapshot not valid after a Full sleep")
	}
}

func TestAFailedLoadKeepsTheSnapshot(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "t5", redis)

	// The next launched fake refuses the load.
	orig := r.l
	r.p.launch = &failingLoad{orig}

	err := r.p.Start(r.ctx, ref)

	var ae *fc.APIError
	if !errors.As(err, &ae) || !strings.Contains(ae.Fault, "memory file") {
		t.Fatalf("err = %v", err)
	}

	if !r.vm(t, ref).SnapshotValid {
		t.Fatal("a load that never ran the guest invalidated the snapshot")
	}

	if orig.server(r.p.dir(ref)) != nil {
		t.Fatal("the failed VMM was left running")
	}
}

type failingLoad struct{ *fakeLauncher }

func (f *failingLoad) Launch(ctx context.Context, s fc.LaunchSpec) (int, error) {
	pid, err := f.fakeLauncher.Launch(ctx, s)
	if err == nil {
		f.server(s.Dir).Fail["/snapshot/load"] = "Cannot open the memory file"
	}

	return pid, err
}

func TestARekeyFailureStopsTheVM(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "t6", redis)

	r.g.failRekey = errors.New("execd did not answer")

	err := r.p.Start(r.ctx, ref)
	if err == nil || !strings.Contains(err.Error(), "re-keying") {
		t.Fatalf("err = %v", err)
	}

	if r.l.server(r.p.dir(ref)) != nil {
		t.Fatal("a VM that could not be re-keyed was left serving")
	}

	// It ran after the load, so its disk may have moved: the next wake is a cold boot.
	if r.vm(t, ref).SnapshotValid {
		t.Fatal("snapshot still valid after the guest ran")
	}
}

func TestASealFailureLeavesTheVMRunning(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "t7", redis)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	r.g.failSeal = errors.New("execd busy")

	if err := r.p.Stop(r.ctx, ref); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("err = %v", err)
	}

	if s := r.l.server(r.p.dir(ref)); s == nil || s.State() != "Running" {
		t.Fatal("an unsealed VM was snapshotted or killed")
	}
}

func TestWithoutAGuestTheLifecycleStillWorksAndExecIsRefused(t *testing.T) {
	r := newRig(t)
	r.p.guest = fc.NoGuest{}

	ref := r.create(t, "t8", redis)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if _, err := r.p.Exec(r.ctx, ref, []string{"true"}); !errors.Is(err, fc.ErrGuestUnavailable) {
		t.Fatalf("Exec = %v", err)
	}

	if err := r.p.ExecTTY(r.ctx, ref, []string{"sh"}); !errors.Is(err, fc.ErrGuestUnavailable) {
		t.Fatalf("ExecTTY = %v", err)
	}

	if err := r.p.Copy(r.ctx, ref, ":/etc/hosts", "/tmp/x"); !errors.Is(err, fc.ErrGuestUnavailable) {
		t.Fatalf("Copy = %v", err)
	}

	if _, err := r.p.DialGuestPort(r.ctx, "t8", "cache", 6379); !errors.Is(err, fc.ErrGuestUnavailable) {
		t.Fatalf("DialGuestPort = %v", err)
	}
}

func TestPauseAndUnpause(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "t9", redis)

	if err := r.p.Pause(r.ctx, ref); err == nil {
		t.Fatal("paused a sleeping VM")
	}

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	for range 2 { // idempotent
		if err := r.p.Pause(r.ctx, ref); err != nil {
			t.Fatal(err)
		}
	}

	if units, _ := r.p.List(r.ctx, "t9"); !units[0].Paused || units[0].Running {
		t.Fatalf("unit = %+v", units[0])
	}

	// Start on a paused VM resumes it, which is what the wake path does on a dial.
	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if units, _ := r.p.List(r.ctx, "t9"); !units[0].Running {
		t.Fatal("Start did not resume a paused VM")
	}

	if err := r.p.Unpause(r.ctx, ref); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTakesEverything(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "t10", redis)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Remove(r.ctx, "t10"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(r.p.dir(ref)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("VM directory survived Remove")
	}

	if len(r.n.taps) != 0 || len(r.n.bridges) != 0 || r.l.server(r.p.dir(ref)) != nil {
		t.Fatalf("left behind: taps %v bridges %v", r.n.taps, r.n.bridges)
	}

	if err := r.p.Remove(r.ctx, "t10"); err == nil {
		t.Fatal("removing an absent sandbox succeeded")
	}
}

func TestSlotsAreStableAndDistinct(t *testing.T) {
	r := newRig(t)
	r.create(t, "a", redis)

	s1, _ := r.p.AllocSlot(r.ctx, "a")
	s2, _ := r.p.AllocSlot(r.ctx, "b")

	if s1 == s2 {
		t.Fatalf("two sandboxes in slot %d", s1)
	}
}

func TestLimits(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "t11", redis)

	l, err := r.p.Limits(r.ctx, ref)
	if err != nil || l.NanoCPUs != 1e9 || l.MemBytes != 256<<20 {
		t.Fatalf("limits = %+v, %v", l, err)
	}

	if err := r.p.SetLimits(r.ctx, ref, Limits{NanoCPUs: 2e9}); err == nil || !strings.Contains(err.Error(), "fixed at boot") {
		t.Fatalf("SetLimits = %v", err)
	}
}

func TestSizing(t *testing.T) {
	for _, tc := range []struct {
		cpu, mem  string
		vcpu, mib int
		bad       bool
	}{
		{"", "", 1, 256, false},
		{"0.5", "", 1, 256, false},
		{"1", "1g", 1, 1024, false},
		{"2", "", 2, 256, false},
		{"3", "", 4, 256, false}, // Firecracker: 1 or even
		{"33", "", 0, 0, true},
		{"x", "", 0, 0, true},
	} {
		v, m, err := sizing(spec.Service{CPU: tc.cpu, Memory: tc.mem})
		if (err != nil) != tc.bad || (!tc.bad && (v != tc.vcpu || m != tc.mib)) {
			t.Errorf("sizing(%q, %q) = %d, %d, %v", tc.cpu, tc.mem, v, m, err)
		}
	}
}

func TestUnsupportedFieldsAreRefusedNotIgnored(t *testing.T) {
	for field, svc := range map[string]spec.Service{
		"build":        {Build: &spec.Build{}},
		"files":        {Files: map[string]string{"a": "b"}},
		"mounts":       {Mounts: map[string]string{"/a": "/b"}},
		"init":         {Init: []string{"x"}},
		"gpus":         {GPUs: "all"},
		"cap_add":      {CapAdd: []string{"NET_ADMIN"}},
		"egress_allow": {EgressAllow: []string{"example.com"}},
	} {
		if err := unsupported(svc); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v", field, err)
		}
	}

	if err := unsupported(spec.Service{Image: "x", Ports: []int{1}, Egress: "deny", Health: "true"}); err != nil {
		t.Errorf("a plain service refused: %v", err)
	}
}

func TestCommitAndRestoreAsTheSameService(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "t12", redis)
	dir := r.p.dir(ref)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	const img = "sbx-snap-s1-cache:latest"

	if err := r.p.Commit(r.ctx, ref, img, "ENV X="); err == nil {
		t.Fatal("image-config changes accepted on a memory snapshot")
	}

	if err := r.p.Commit(r.ctx, ref, img); err != nil {
		t.Fatal(err)
	}

	got := pathsOf(r.l.server(dir).Calls())
	if !slices.Equal(got[len(got)-3:], []string{"PATCH /vm", "PUT /snapshot/create", "PATCH /vm"}) {
		t.Fatalf("commit calls = %v", got)
	}

	if r.l.server(dir).State() != "Running" {
		t.Fatal("Commit left the VM paused")
	}

	// The commit reset Firecracker's dirty bitmap, so the next sleep must be Full.
	if r.vm(t, ref).Restored {
		t.Fatal("Restored still set after a commit; the next Diff would miss pages")
	}

	merges := r.merge
	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if r.merge != merges {
		t.Fatal("slept as a Diff after a commit")
	}

	imgs, err := r.p.Images(r.ctx, "sbx-snap-s1")
	if err != nil || !slices.Equal(imgs, []string{img}) {
		t.Fatalf("Images = %v, %v", imgs, err)
	}

	for _, f := range []string{fc.StateName, fc.MemName, fc.RootfsName, "agent.ext4"} {
		if _, err := os.Stat(filepath.Join(r.p.snapshotDir(img), f)); err != nil {
			t.Fatalf("snapshot lacks %s", f)
		}
	}

	if r.p.VolumeFor("t12", "cache") != "" {
		t.Fatal("a VM reported a separate data volume")
	}

	slot := r.vm(t, ref).Slot

	if err := r.p.Remove(r.ctx, "t12"); err != nil {
		t.Fatal(err)
	}

	// Under another name it would be a fork: refused, saying why.
	eps := r.p.Endpoints("other", "cache", slot, 0, redis.Ports)
	svc := redis
	svc.Image = img

	if err := r.p.Create(r.ctx, "other", slot, 0, "cache", svc, eps, "", IsolationContainer); err == nil ||
		!strings.Contains(err.Error(), "same sandbox") {
		t.Fatalf("fork = %v", err)
	}

	// In another slot the guest's address would be wrong: refused.
	if err := r.p.Create(r.ctx, "t12", slot+1, 0, "cache", svc, eps, "", IsolationContainer); err == nil ||
		!strings.Contains(err.Error(), "slot") {
		t.Fatalf("other slot = %v", err)
	}

	// As itself: born asleep with a valid snapshot, and the first wake is a load.
	if err := r.p.Create(r.ctx, "t12", slot, 0, "cache", svc, eps, "", IsolationContainer); err != nil {
		t.Fatal(err)
	}

	if !r.vm(t, ref).SnapshotValid {
		t.Fatal("restored service not born asleep")
	}

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if got := pathsOf(r.l.server(dir).Calls()); !slices.Contains(got, "PUT /snapshot/load") {
		t.Fatalf("restored service cold-booted: %v", got)
	}

	if err := r.p.RemoveImage(r.ctx, img); err != nil {
		t.Fatal(err)
	}

	if imgs, _ := r.p.Images(r.ctx, ""); len(imgs) != 0 {
		t.Fatalf("image survived RemoveImage: %v", imgs)
	}
}

func TestSelectionFollowsHostcap(t *testing.T) {
	saved, savedHelper := probeHost, HelperVMProvider
	t.Cleanup(func() { probeHost, HelperVMProvider = saved, savedHelper })

	probeHost = func() hostcap.Report {
		return hostcap.Report{OS: "darwin", Arch: "arm64", CPUBrand: "Apple M4", OSVersion: "26.4.1", Nested: true, NestedHint: "Apple M4, macOS 26.4.1"}
	}

	HelperVMProvider = nil

	if _, err := For("firecracker", "", ""); err == nil || !strings.Contains(err.Error(), "helper-VM layer") {
		t.Fatalf("helper-vm without the layer = %v", err)
	}

	sentinel := errors.New("helper layer reached")
	HelperVMProvider = func(d hostcap.Decision) (Provider, error) {
		if d.Backend != hostcap.HelperVM {
			t.Errorf("decision = %+v", d)
		}

		return nil, sentinel
	}

	if _, err := For("fc", "", ""); !errors.Is(err, sentinel) {
		t.Fatalf("helper layer not consulted: %v", err)
	}

	probeHost = func() hostcap.Report {
		return hostcap.Report{OS: "linux", Arch: "amd64", KVM: hostcap.KVM{Detail: "/dev/kvm does not exist"}}
	}

	if _, err := For("firecracker", "", ""); err == nil || !strings.Contains(err.Error(), "modprobe") {
		t.Fatalf("no kvm = %v", err)
	}

	if _, err := For("nope", "", ""); err == nil || !strings.Contains(err.Error(), "firecracker") {
		t.Fatalf("unknown provider message does not list firecracker: %v", err)
	}
}

// The control secret execd holds moves on at every re-key, and every later Seal and Rekey must
// present the one it holds now - the snapshot's - or execd answers UNAUTHORIZED and the VM can
// never be slept or woken again. A cold boot is back to the secret baked into the agent drive.
func TestTheControlSecretFollowsExecd(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "t9", redis)
	boot := r.vm(t, ref).ControlSecret

	var secrets []string

	r.g.onSeal = func(s string) { secrets = append(secrets, s) }

	for range 2 {
		if err := r.p.Start(r.ctx, ref); err != nil {
			t.Fatal(err)
		}

		if err := r.p.Stop(r.ctx, ref); err != nil {
			t.Fatal(err)
		}
	}

	if len(r.g.rekeys) != 2 {
		t.Fatalf("rekeys = %+v", r.g.rekeys)
	}

	first, second := r.g.rekeys[0], r.g.rekeys[1]
	if first.Secret != boot || second.Secret != first.ControlSecret || second.ControlSecret == first.ControlSecret {
		t.Fatalf("re-key chain broken: boot %q, then %+v, then %+v", boot, first, second)
	}

	if !slices.Equal(secrets, []string{first.ControlSecret, second.ControlSecret}) {
		t.Fatalf("seals presented %q, want each wake's rotated secret", secrets)
	}

	// Dies awake: the next Start cold-boots, and that execd holds the boot secret again.
	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	_ = r.l.Kill(r.ctx, r.p.dir(ref))

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if last := secrets[len(secrets)-1]; last != boot {
		t.Fatalf("after a cold boot the seal presented %q, want the boot secret", last)
	}
}

// In the helper VM the provider can probe only the VM's loopback, but the Mac mirrors every
// sandbox port at the same number on ITS loopback - where a docker sandbox may already hold it.
// The Mac says which slots are taken there, and the provider must not hand one out.
func TestSlotsTheHostHoldsAreSkipped(t *testing.T) {
	r := newRig(t)
	t.Setenv(HostBusySlotsEnv, "0,1, 3")

	s, err := r.p.AllocSlot(r.ctx, "a")
	if err != nil || s != 2 {
		t.Fatalf("slot = %d, %v; want 2, the first the host does not hold", s, err)
	}

	r.create(t, "a", redis)

	if s, _ := r.p.AllocSlot(r.ctx, "b"); s != 4 {
		t.Fatalf("second slot = %d, want 4", s)
	}

	// A sandbox that already has a slot keeps it, whatever the host says now.
	t.Setenv(HostBusySlotsEnv, "2")

	if s, _ := r.p.AllocSlot(r.ctx, "a"); s != 2 {
		t.Fatalf("an existing sandbox moved to slot %d", s)
	}
}

func TestParseSlots(t *testing.T) {
	got := parseSlots(" 5,x,,7 ,-1,128")
	if len(got) != 2 || !got[5] || !got[7] {
		t.Fatalf("parseSlots = %v, want {5,7}: junk and out-of-range numbers ignored", got)
	}
}
