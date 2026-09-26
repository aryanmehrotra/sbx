package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// captureExt4 is touchExt4 that also keeps every agent drive's init.json, by the image it built.
type captureExt4 struct {
	mu    sync.Mutex
	inits map[string]fc.InitConfig
}

func (*captureExt4) TakesTar(context.Context) bool { return true }

func (c *captureExt4) Build(_ context.Context, src, img, _ string) error {
	if b, err := os.ReadFile(filepath.Join(src, "init.json")); err == nil {
		var cfg fc.InitConfig
		if err := json.Unmarshal(b, &cfg); err != nil {
			return err
		}

		c.mu.Lock()
		c.inits[img] = cfg
		c.mu.Unlock()
	}

	return os.WriteFile(img, []byte("ext4"), 0o600)
}

func (c *captureExt4) init(t *testing.T, dir string) fc.InitConfig {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, ok := c.inits[filepath.Join(dir, "agent.ext4")]
	if !ok {
		t.Fatalf("no agent drive was built in %s", dir)
	}

	return cfg
}

func osbRig(t *testing.T) (*rig, *captureExt4) {
	t.Helper()

	r := newRig(t)
	x := &captureExt4{inits: map[string]fc.InitConfig{}}
	r.p.ext4 = x

	return r, x
}

// apiSvc is what the OpenSandbox API asks a RunsAgent provider for.
func apiSvc(token string) spec.Service {
	return spec.Service{Image: "python:3.11-slim", Ports: []int{44772}, OSBOwner: "sbx-serve",
		Entrypoint: []string{"tail", "-f", "/dev/null"}, OnIdle: spec.OnIdleFreeze,
		Env: map[string]string{AgentTokenEnv: token, "A": "1"}}
}

func envOf(cfg fc.InitConfig, key string) []string {
	var out []string

	for _, kv := range cfg.Env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			out = append(out, v)
		}
	}

	return out
}

// An API sandbox is born running with the API's token - once, and no other - and stays that
// token across a sleep and a wake.
func TestAnAPISandboxIsBornRunningWithTheAPIsToken(t *testing.T) {
	r, x := osbRig(t)

	ref := r.create(t, "osb-1", apiSvc("tok-from-the-api"))
	dir := r.p.dir(ref)

	vm := r.vm(t, ref)
	if vm.AccessToken != "tok-from-the-api" || vm.OSB != "sbx-serve" || vm.Config == nil {
		t.Fatalf("record = %+v", vm)
	}

	cfg := x.init(t, dir)
	if got := envOf(cfg, AgentTokenEnv); !slices.Equal(got, []string{"tok-from-the-api"}) {
		t.Fatalf("the guest's %s = %v, want exactly the API's", AgentTokenEnv, got)
	}

	if !slices.Equal(cfg.Argv, []string{"tail", "-f", "/dev/null"}) || !slices.Contains(cfg.Env, "A=1") {
		t.Fatalf("init = %+v", cfg)
	}

	// Running, never snapshotted: no process was killed and no snapshot was written.
	if s := r.l.server(dir); s == nil || s.State() != fc.StateRunning || r.l.kills != 0 {
		t.Fatalf("after create: server=%v kills=%d, want a running VM", s, r.l.kills)
	}

	if vm.SnapshotValid {
		t.Fatal("SnapshotValid on a VM that was never snapshotted")
	}

	units, _ := r.p.List(r.ctx, "osb-1")
	if len(units) != 1 || !units[0].Running || units[0].OSB != "sbx-serve" {
		t.Fatalf("units = %+v", units)
	}

	// Asleep and awake again: the re-key hands execd the API's token back.
	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if n := len(r.g.rekeys); n == 0 || r.g.rekeys[n-1].AccessToken != "tok-from-the-api" {
		t.Fatalf("rekeys = %+v", r.g.rekeys)
	}
}

// A sandbox.json service has no API to mint its token, so the provider still does, and it is
// still born asleep.
func TestASpecServiceStillGetsItsOwnTokenAndSleeps(t *testing.T) {
	r, x := osbRig(t)

	ref := r.create(t, "plain", redis)
	vm := r.vm(t, ref)

	if len(vm.AccessToken) != 32 || vm.OSB != "" || !vm.SnapshotValid || r.l.server(r.p.dir(ref)) != nil {
		t.Fatalf("record = %+v", vm)
	}

	if got := envOf(x.init(t, r.p.dir(ref)), AgentTokenEnv); !slices.Equal(got, []string{vm.AccessToken}) {
		t.Fatalf("token env = %v", got)
	}
}

// A pvc is an ext4 image attached as an extra drive and mounted by fc-init; one VM at a time.
func TestNamedVolumesAreDrivesOneVMAtATime(t *testing.T) {
	r, x := osbRig(t)
	ctx := r.ctx

	if err := r.p.CreateVolume(ctx, "sbx-osb-pvc-data", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}

	if ok, err := r.p.VolumeExists(ctx, "sbx-osb-pvc-data"); !ok || err != nil {
		t.Fatalf("exists = %v, %v", ok, err)
	}

	if st, err := os.Stat(r.p.volumePath("sbx-osb-pvc-data")); err != nil || st.Size() == 0 {
		t.Fatalf("volume image: %v", err)
	}

	for _, bad := range []string{"../x", "a/b", "", ".hidden"} {
		if err := r.p.CreateVolume(ctx, bad, nil); err == nil {
			t.Errorf("CreateVolume(%q) was accepted", bad)
		}
	}

	var seenDrives []map[string]any

	r.p.ready = func(_ context.Context, _ *fcVM, dir string) error {
		for _, c := range r.l.server(dir).Calls() {
			if strings.HasPrefix(c.Path, "/drives/vol") {
				seenDrives = append(seenDrives, c.Body)
			}
		}

		return nil
	}

	svc := apiSvc("t")
	svc.VolumeMounts = []spec.VolumeMount{{Volume: "sbx-osb-pvc-data", Target: "/data", SubPath: "x", ReadOnly: true}}
	ref := r.create(t, "vol-1", svc)

	if len(seenDrives) != 1 || seenDrives[0]["path_on_host"] != r.p.volumePath("sbx-osb-pvc-data") ||
		seenDrives[0]["is_read_only"] != true || seenDrives[0]["is_root_device"] != false {
		t.Fatalf("volume drives = %v", seenDrives)
	}

	want := []fc.InitMount{{Device: "/dev/vdc", Target: "/data", SubPath: "x", ReadOnly: true}}
	if got := x.init(t, r.p.dir(ref)).Mounts; !slices.Equal(got, want) {
		t.Fatalf("init mounts = %+v", got)
	}

	// A second VM may not attach it, and it cannot be removed while attached.
	slot, _ := r.p.AllocSlot(ctx, "vol-2")
	err := r.p.Create(ctx, "vol-2", slot, 0, "cache", svc, r.p.Endpoints("vol-2", "cache", slot, 0, svc.Ports), "", IsolationContainer)

	if err == nil || !strings.Contains(err.Error(), "attached to "+ref) {
		t.Fatalf("second attach = %v", err)
	}

	if err := r.p.RemoveVolume(ctx, "sbx-osb-pvc-data"); err == nil || !strings.Contains(err.Error(), "attached") {
		t.Fatalf("remove while attached = %v", err)
	}

	// Mounted twice in one VM is refused too: one kernel, one mount of a block device.
	twice := apiSvc("t")
	twice.VolumeMounts = []spec.VolumeMount{{Volume: "sbx-osb-pvc-other", Target: "/a"}, {Volume: "sbx-osb-pvc-other", Target: "/b"}}

	if err := r.p.CreateVolume(ctx, "sbx-osb-pvc-other", nil); err != nil {
		t.Fatal(err)
	}

	slot, _ = r.p.AllocSlot(ctx, "vol-3")
	if err := r.p.Create(ctx, "vol-3", slot, 0, "cache", twice, nil, "", IsolationContainer); err == nil ||
		!strings.Contains(err.Error(), "mounted twice") {
		t.Fatalf("twice = %v", err)
	}

	// A missing volume is named.
	missing := apiSvc("t")
	missing.VolumeMounts = []spec.VolumeMount{{Volume: "sbx-osb-pvc-nope", Target: "/n"}}

	slot, _ = r.p.AllocSlot(ctx, "vol-4")
	if err := r.p.Create(ctx, "vol-4", slot, 0, "cache", missing, nil, "", IsolationContainer); err == nil ||
		!strings.Contains(err.Error(), "no volume sbx-osb-pvc-nope") {
		t.Fatalf("missing = %v", err)
	}

	// Gone with its VM, it can be removed.
	if err := r.p.Remove(ctx, "vol-1"); err != nil {
		t.Fatal(err)
	}

	if err := r.p.RemoveVolume(ctx, "sbx-osb-pvc-data"); err != nil {
		t.Fatal(err)
	}

	if ok, _ := r.p.VolumeExists(ctx, "sbx-osb-pvc-data"); ok {
		t.Fatal("volume still there")
	}
}

// A host directory is still refused by name; a named volume is not.
func TestOnlyHostMountsAreRefused(t *testing.T) {
	host := spec.Service{Image: "x", VolumeMounts: []spec.VolumeMount{{Host: "/srv", Target: "/srv"}}}
	if err := unsupported(host); err == nil || !strings.Contains(err.Error(), "mounts") {
		t.Fatalf("host mount = %v", err)
	}

	named := spec.Service{Image: "x", VolumeMounts: []spec.VolumeMount{{Volume: "v", Target: "/v"}}}
	if err := unsupported(named); err != nil {
		t.Fatalf("named volume refused: %v", err)
	}
}

// An API sandbox's snapshot is its disk, synced first and copied while paused, with no identity;
// a sandbox created from it is a new VM, cold-booted from a copy, with its own token.
func TestAnAPISnapshotIsTheDiskAndForksColdWithItsOwnIdentity(t *testing.T) {
	r, x := osbRig(t)

	var execs [][]string

	r.p.healthExec = func(_ context.Context, ref string, argv []string) (string, error) {
		if s := r.l.server(r.p.dir(ref)); s == nil || s.State() != fc.StateRunning {
			t.Errorf("fssync ran on a VM that is not running")
		}

		execs = append(execs, argv)

		return "", nil
	}

	ref := r.create(t, "src", apiSvc("src-token"))
	srcDir := r.p.dir(ref)

	if err := os.WriteFile(filepath.Join(srcDir, fc.RootfsName), []byte("the source's disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	calls := len(r.l.server(srcDir).Calls())

	if err := r.p.Commit(r.ctx, ref, "sbx-osb-snap:snap-1"); err != nil {
		t.Fatal(err)
	}

	if len(execs) != 1 || !slices.Equal(execs[0], []string{fc.GuestAgentPath, "fssync"}) {
		t.Fatalf("execs = %v, want the agent's fssync before the copy", execs)
	}

	var states []string

	for _, c := range r.l.server(srcDir).Calls()[calls:] {
		if c.Path == "/vm" {
			states = append(states, c.Body["state"].(string))
		}

		if strings.HasPrefix(c.Path, "/snapshot") {
			t.Fatalf("a memory snapshot was taken: %s", c.Path)
		}
	}

	if !slices.Equal(states, []string{"Paused", "Resumed"}) {
		t.Fatalf("vm states around the copy = %v", states)
	}

	snapDir, ok := r.p.savedSnapshotDir("sbx-osb-snap:snap-1")
	if !ok {
		t.Fatal("no snapshot")
	}

	for _, f := range []string{fc.StateName, fc.MemName, "agent.ext4"} {
		if _, err := os.Stat(filepath.Join(snapDir, f)); err == nil {
			t.Errorf("%s in a disk snapshot", f)
		}
	}

	raw, _ := os.ReadFile(filepath.Join(snapDir, "snapshot.json"))
	if strings.Contains(string(raw), "src-token") || strings.Contains(string(raw), r.vm(t, ref).ControlSecret) {
		t.Fatalf("the snapshot record holds the source's identity: %s", raw)
	}

	info, err := r.p.ImageInfo(r.ctx, "sbx-osb-snap:snap-1")
	if err != nil || !slices.Equal(info.Cmd, []string{"redis-server"}) || info.OS != "linux" {
		t.Fatalf("ImageInfo(snapshot) = %+v, %v", info, err)
	}

	// The fork.
	fork := apiSvc("fork-token")
	fork.Image = "sbx-osb-snap:snap-1"
	fref := r.create(t, "fork", fork)
	fdir := r.p.dir(fref)

	if b, _ := os.ReadFile(filepath.Join(fdir, fc.RootfsName)); string(b) != "the source's disk" {
		t.Fatalf("fork rootfs = %q", b)
	}

	fvm := r.vm(t, fref)
	if fvm.AccessToken != "fork-token" || fvm.Slot == r.vm(t, ref).Slot || fvm.Restored {
		t.Fatalf("fork record = %+v", fvm)
	}

	if got := envOf(x.init(t, fdir), AgentTokenEnv); !slices.Equal(got, []string{"fork-token"}) {
		t.Fatalf("fork token env = %v", got)
	}

	for _, c := range r.l.server(fdir).Calls() {
		if c.Path == "/snapshot/load" {
			t.Fatal("the fork restored memory; it must cold-boot")
		}
	}
}

// Committed while frozen, it stays frozen - resumed only for the sync.
func TestAnAPISnapshotOfAFrozenSandboxLeavesItFrozen(t *testing.T) {
	r, _ := osbRig(t)
	r.p.healthExec = func(context.Context, string, []string) (string, error) { return "", nil }

	ref := r.create(t, "frozen", apiSvc("t"))

	if err := r.p.Pause(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Commit(r.ctx, ref, "sbx-osb-snap:snap-2"); err != nil {
		t.Fatal(err)
	}

	if s := r.l.server(r.p.dir(ref)); s.State() != fc.StatePaused {
		t.Fatalf("state after commit = %s", s.State())
	}
}

func TestImageInfoAndPullGoThroughTheEngine(t *testing.T) {
	r, _ := osbRig(t)

	info, err := r.p.ImageInfo(r.ctx, "redis:7")
	if err != nil || !slices.Equal(info.Cmd, []string{"redis-server"}) {
		t.Fatalf("ImageInfo = %+v, %v", info, err)
	}

	if err := r.p.Pull(r.ctx, "redis:7"); err != nil {
		t.Fatal(err)
	}
}
