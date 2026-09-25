package provider

// The firecracker provider: each service is a microVM, and asleep means a snapshot on disk.
//
// The wake verbs change meaning, and that is the whole point (ROADMAP §1). Create builds the
// root filesystem, boots the VM once, waits for it to serve, and snapshots it - so a new service
// is born asleep, like every other. Start loads the snapshot and resumes: memory and processes
// come back as they were, rather than a cold process against a warm disk. Stop snapshots again
// and kills the VMM: a Diff when this VM was itself restored (only the pages it dirtied), folded
// into the base so the next load is still one file.
//
// Everything is on disk, under one directory per VM, and nothing is held in this process: the
// VM that `sbx create` boots is stopped by `sbx serve` and woken by it again after a restart,
// possibly inside the helper VM on a Mac, where this same code runs one level down. So a method
// here never assumes it started what it is looking at; it asks the API socket.
//
// A snapshot is only valid against the disk as it was when the snapshot was taken. The moment a
// restored VM runs, its disk moves on and the snapshot describes the past; if that VM then dies
// without being slept - a host reboot, a crash, a workload that exits - restoring the old memory
// over the newer disk would hand the guest a page cache that disagrees with its filesystem. So
// the snapshot is marked invalid, durably, before every resume, and valid again only after the
// next snapshot completes; a Start that finds it invalid cold-boots against the disk as it is,
// which is a power loss the guest's ext4 journal knows how to recover from.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/agentbin"
	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// HelperVMProvider is where the helper-VM layer (internal/fchost: sbx inside a nested-virtualisation
// Linux VM, for macOS and Windows) plugs in, from its init. hostcap decides the path; a build
// without that layer reports a helper-vm decision as the one thing missing rather than as "this
// machine cannot".
var HelperVMProvider func(d hostcap.Decision) (Provider, error)

// probeHost is hostcap.Probe; a variable so the selection path is tested for every host shape.
var probeHost = hostcap.Probe

// Version is the sbx build version, used to find the agent binary; app sets it.
var Version = "dev"

func forFirecracker(socket string) (Provider, error) {
	d := hostcap.Decide(probeHost())

	switch d.Backend {
	case hostcap.Direct:
		ep, err := resolveDockerHost(socket)
		if err != nil {
			return nil, fmt.Errorf("the firecracker provider builds root filesystems through "+
				"docker, and no engine was found: %w", err)
		}

		return newFirecracker(ep.String())
	case hostcap.HelperVM:
		if HelperVMProvider != nil {
			return HelperVMProvider(d)
		}

		return nil, fmt.Errorf("firecracker here goes through a Linux helper VM (%s), and this "+
			"build does not carry the helper-VM layer yet - use --provider docker, or run sbx "+
			"on a Linux host with /dev/kvm", d.Reason)
	case hostcap.KataRuntimeClass:
		return nil, fmt.Errorf("%s: use --provider kubernetes --isolation kata", d.Reason)
	default:
		return nil, d.Err()
	}
}

// fcVM is one VM's record, the file every sbx process agrees on.
type fcVM struct {
	Sandbox   string    `json:"sandbox"`
	Service   string    `json:"service"`
	Ref       string    `json:"ref"`
	Instance  string    `json:"instance"` // random per create: a re-created service is a new one
	Slot      int       `json:"slot"`
	Index     int       `json:"index"`
	Image     string    `json:"image"`
	ImageID   string    `json:"image_id"`
	VCPU      int       `json:"vcpu"`
	MemMiB    int       `json:"mem_mib"`
	Ports     []int     `json:"ports"`  // inside the guest
	Public    []int     `json:"public"` // where clients connect
	DependsOn []string  `json:"depends_on,omitempty"`
	Idle      string    `json:"idle,omitempty"`
	OnIdle    string    `json:"on_idle,omitempty"`
	Kernel    string    `json:"kernel"`
	Binary    string    `json:"firecracker"`
	Clone     string    `json:"clone"` // how the rootfs was cloned: reflink or copy
	Created   time.Time `json:"created"`

	// Identity the guest agent holds. On disk because every process that wakes this VM must
	// hand the same token back to it; 0600 like the rest of the directory. ControlSecret is the
	// boot secret, baked into the agent drive; LiveSecret is the one execd holds now (running,
	// or captured in the snapshot), which every re-key rotates. Empty means the boot secret.
	AccessToken   string `json:"access_token"`
	ControlSecret string `json:"control_secret"`
	Generation    uint64 `json:"generation"`
	LiveSecret    string `json:"live_secret,omitempty"`

	// SnapshotValid: vm.state + vm.mem describe the disk as it is now. See the file comment.
	SnapshotValid bool `json:"snapshot_valid"`

	// Restored: the running process was loaded from vm.mem with dirty tracking, so a Diff on
	// top of vm.mem is a correct snapshot. False after a cold boot, and after a Commit, which
	// resets Firecracker's dirty bitmap and would make the next Diff miss pages.
	Restored bool `json:"restored"`
}

type fcProvider struct {
	root   string // <state>/fc
	arch   string
	arts   *fc.ArtifactCache
	rootfs *fc.RootfsBuilder
	ext4   fc.Ext4Builder
	net    fc.Network
	launch fc.Launcher
	guest  fc.Guest

	agent func(ctx context.Context, arch string) (string, error)

	// merge folds a Diff memory file into its base; fc.MergeDiff, which is Linux-only.
	merge func(diff, base string) error

	// ready waits until a freshly booted VM is serving; a field so tests need no guest network.
	ready func(ctx context.Context, vm *fcVM, dir string) error

	bootTimeout time.Duration

	// portsFree probes a slot's public ports; publicPortsFree, a field so tests do not depend on
	// what else this machine is listening on.
	portsFree func(slot int) bool

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func stateRoot() (string, error) {
	if d := os.Getenv("SBX_FC_STATE"); d != "" {
		return d, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory for the firecracker state; set SBX_FC_STATE: %w", err)
	}

	return filepath.Join(home, ".sbx", "fc"), nil
}

func newFirecracker(dockerHost string) (*fcProvider, error) {
	root, err := stateRoot()
	if err != nil {
		return nil, err
	}

	rep := hostcap.Probe()
	mkfs := fc.Mkfs{Path: rep.Mkfs}

	p := &fcProvider{
		root:  root,
		arch:  runtime.GOARCH,
		arts:  fc.NewArtifactCache(filepath.Join(root, "artifacts")),
		ext4:  mkfs,
		net:   fc.NewIPNetwork(os.Getuid()),
		guest: fc.NewGuest(),
		rootfs: &fc.RootfsBuilder{
			Dir:    filepath.Join(root, "rootfs"),
			Engine: fc.DockerCLI{Env: []string{"DOCKER_HOST=" + dockerHost}},
			Ext4:   mkfs,
			Root:   os.Geteuid() == 0,
		},
		launch:      fc.ExecLauncher{},
		bootTimeout: 60 * time.Second,
		locks:       map[string]*sync.Mutex{},
	}

	p.agent = p.findAgent
	p.ready = p.waitServing
	p.merge = fc.MergeDiff
	p.portsFree = publicPortsFree

	if v := os.Getenv("SBX_FC_BOOT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			p.bootTimeout = d
		}
	}

	return p, nil
}

func (p *fcProvider) Name() string { return "firecracker" }

// dir is keyed by a hash of the ref, not the ref itself: the API socket lives inside it, and a
// unix socket path is capped at 108 bytes, which a long sandbox name under a long $HOME passes.
func (p *fcProvider) dir(ref string) string {
	h := sha256.Sum256([]byte(ref))
	return filepath.Join(p.root, "vms", hex.EncodeToString(h[:8]))
}

func (p *fcProvider) lock(ref string) func() {
	p.mu.Lock()

	l, ok := p.locks[ref]
	if !ok {
		l = &sync.Mutex{}
		p.locks[ref] = l
	}

	p.mu.Unlock()
	l.Lock()

	// And across processes: `sbx serve` sleeping a VM while `sbx rm` removes it.
	unlock := fileLock(filepath.Join(p.dir(ref), fc.LockFileName))

	return func() { unlock(); l.Unlock() }
}

func (p *fcProvider) load(ref string) (*fcVM, error) {
	b, err := os.ReadFile(filepath.Join(p.dir(ref), "vm.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no firecracker service %q", ref)
	}

	if err != nil {
		return nil, err
	}

	var vm fcVM
	if err := json.Unmarshal(b, &vm); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", filepath.Join(p.dir(ref), "vm.json"), err)
	}

	return &vm, nil
}

// save writes the record atomically and durably. Durably because SnapshotValid=false must be on
// disk before a resume lets the disk diverge; a crash that forgot it would restore stale memory.
func (p *fcProvider) save(vm *fcVM) error {
	b, err := json.MarshalIndent(vm, "", "  ")
	if err != nil {
		return err
	}

	dir := p.dir(vm.Ref)
	tmp := filepath.Join(dir, "vm.json.tmp")

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}

	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	return os.Rename(tmp, filepath.Join(dir, "vm.json"))
}

func (vm *fcVM) addr() fc.Addr { return fc.Addr{Slot: vm.Slot, Index: vm.Index} }

// secret is the control secret execd holds now.
func (vm *fcVM) secret() string {
	if vm.LiveSecret != "" {
		return vm.LiveSecret
	}

	return vm.ControlSecret
}

func (p *fcProvider) guestVM(vm *fcVM) fc.GuestVM {
	dir := p.dir(vm.Ref)
	return fc.GuestVM{Sandbox: vm.Sandbox, Service: vm.Service, Dir: dir, VsockUDS: filepath.Join(dir, fc.VsockName)}
}

func (p *fcProvider) client(ref string) *fc.Client {
	return fc.NewClient(filepath.Join(p.dir(ref), fc.APISockName))
}

// Endpoints is docker's addressing: the daemon fronts the same per-slot port block, so a client
// cannot tell which provider a sandbox is on. Only the upstream differs - the guest's own IP.
func (p *fcProvider) Endpoints(_, _ string, slot, startIndex int, containerPorts []int) []Endpoint {
	eps := make([]Endpoint, 0, len(containerPorts))
	for i := range containerPorts {
		eps = append(eps, Endpoint{Host: "127.0.0.1", Port: publicBase + slot*blockSize + startIndex + i})
	}

	return eps
}

func (p *fcProvider) AllocSlot(ctx context.Context, sandbox string) (int, error) {
	vms, err := p.all()
	if err != nil {
		return 0, err
	}

	used := map[int]bool{}

	for _, vm := range vms {
		if vm.Sandbox == sandbox {
			return vm.Slot, nil
		}

		used[vm.Slot] = true
	}

	for i := range maxSlots {
		if used[i] || !p.portsFree(i) {
			continue
		}

		return i, nil
	}

	return 0, fmt.Errorf("all %d sandbox slots are in use; destroy one first", maxSlots)
}

// publicPortsFree is the docker provider's probe on the public half only: a VM has no backing
// port on the host. It is also what keeps a firecracker slot off one a docker sandbox holds,
// since the daemon binds every sandbox's public ports whichever provider made it.
func publicPortsFree(slot int) bool {
	for i := range 3 {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(publicBase+slot*blockSize+i))
		if err != nil {
			return false
		}

		_ = ln.Close()
	}

	return true
}

// unsupported names the spec fields this provider cannot honour yet, each with why. Refused,
// never ignored: a field that silently did nothing is worse than one that says no.
func unsupported(svc spec.Service) error {
	var why []string

	add := func(set bool, field, reason string) {
		if set {
			why = append(why, fmt.Sprintf("%s (%s)", field, reason))
		}
	}

	add(svc.Build != nil, "build", "build the image with docker first and name it with `image`")
	add(len(svc.Files) > 0, "files", "needs the guest agent to write into a running VM")
	add(len(svc.Mounts) > 0 || len(svc.VolumeMounts) > 0 || len(svc.ReadOnlyVolumes) > 0,
		"mounts", "a host directory cannot be bind-mounted into a VM; it would be a virtio-fs device")
	add(len(svc.Init) > 0, "init", "needs the guest agent to run commands inside the VM")
	add(svc.GPUs != "", "gpus", "Firecracker has no device passthrough")
	add(len(svc.CapAdd) > 0, "cap_add", "the workload is root in its own kernel; there is no capability set to widen")
	add(len(svc.EgressAllow) > 0 || svc.EgressPolicy != nil, "egress_allow/egress_policy",
		"the egress filter does not listen on a VM bridge yet; a VM has no egress at all, which is egress: deny")
	add(svc.Egress != "" && svc.Egress != "deny", "egress", "a VM bridge has no NAT, so the only egress it has is deny")

	if len(why) == 0 {
		return nil
	}

	return fmt.Errorf("the firecracker provider cannot honour %s - remove them, or use --provider docker",
		strings.Join(why, "; "))
}

// sizing reads cpu and memory from the spec. Firecracker takes whole vCPUs, 1 or an even
// number, so a fractional core rounds up; the default is one vCPU and 256 MiB, which is also the
// size of the snapshot on disk while the service sleeps.
func sizing(svc spec.Service) (int, int, error) {
	l, err := ParseLimits(svc.CPU, svc.Memory)
	if err != nil {
		return 0, 0, err
	}

	vcpu := 1
	if l.NanoCPUs > 0 {
		vcpu = int(math.Ceil(float64(l.NanoCPUs) / 1e9))
		if vcpu > 1 && vcpu%2 == 1 {
			vcpu++
		}
	}

	if vcpu > 32 {
		return 0, 0, fmt.Errorf("cpu %s is more than Firecracker's 32 vCPUs", svc.CPU)
	}

	mem := 256
	if l.MemBytes > 0 {
		mem = int((l.MemBytes + (1<<20 - 1)) >> 20)
	}

	return vcpu, mem, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}

func (p *fcProvider) findAgent(ctx context.Context, arch string) (string, error) {
	src, err := agentbin.Locate(ctx, arch, Version)
	if err != nil {
		return "", err
	}

	if src.File != "" {
		return src.File, nil
	}

	// Only the published image has it: copy it out once, keyed by version.
	dst := filepath.Join(p.root, "agent", Version, "sbx")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}

	eng, ok := p.rootfs.Engine.(fc.DockerCLI)
	if !ok {
		return "", errors.New("no engine to copy the agent out of " + src.Image)
	}

	if err := eng.Pull(ctx, src.Image); err != nil {
		return "", err
	}

	return dst, eng.CopyOut(ctx, src.Image, agentbin.ImagePath, dst)
}

// Create realises a service as a VM and leaves it asleep: snapshotted, no process running.
func (p *fcProvider) Create(ctx context.Context, sandbox string, slot, ordinal int, service string,
	svc spec.Service, eps []Endpoint, _ string, _ Isolation,
) error {
	// Every isolation tier is met: a VM is a stronger boundary than gVisor or a container, and
	// the same boundary kata gives. Nothing is downgraded, so nothing is refused here.
	if err := unsupported(svc); err != nil {
		return err
	}

	vcpu, mem, err := sizing(svc)
	if err != nil {
		return err
	}

	ref := containerName(sandbox, service)
	dir := p.dir(ref)

	if _, err := os.Stat(filepath.Join(dir, "vm.json")); err == nil {
		return fmt.Errorf("%s already exists; sbx rm %s first", ref, sandbox)
	}

	if snap, ok := p.snapshotFor(svc.Image); ok {
		return p.createFromSnapshot(ctx, snap, svc.Image, ref, slot, eps)
	}

	arts, err := p.arts.Resolve(ctx, p.arch)
	if err != nil {
		return err
	}

	if svc.Image == "" {
		return errors.New("the firecracker provider boots an image; the service names none")
	}

	rfs, err := p.rootfs.Build(ctx, svc.Image)
	if err != nil {
		return err
	}

	agent, err := p.agent(ctx, p.arch)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	unlock := p.lock(ref)
	defer unlock()

	// A failure from here on leaves nothing behind: a half-made VM directory would be listed
	// as a service that can never start.
	ok := false
	defer func() {
		if !ok {
			_ = p.launch.Kill(context.WithoutCancel(ctx), dir)
			_ = os.RemoveAll(dir)
		}
	}()

	clone, err := fc.CloneFile(rfs.Path, filepath.Join(dir, fc.RootfsName))
	if err != nil {
		return fmt.Errorf("cloning the root filesystem: %w", err)
	}

	index := blockSize + ordinal // a service with no ports still needs an address
	if len(eps) > 0 {
		index = (eps[0].Port - publicBase) % blockSize
	}

	vm := &fcVM{
		Sandbox: sandbox, Service: service, Ref: ref, Instance: randomHex(8),
		Slot: slot, Index: index, Image: svc.Image, ImageID: rfs.Config.ID,
		VCPU: vcpu, MemMiB: mem, Ports: svc.Ports, DependsOn: svc.DependsOn,
		Idle: svc.Idle, OnIdle: svc.OnIdle, Kernel: arts.Kernel, Binary: arts.Firecracker,
		Clone: clone, Created: time.Now().UTC(),
		AccessToken: randomHex(16), ControlSecret: randomHex(32),
	}

	for _, e := range eps {
		vm.Public = append(vm.Public, e.Port)
	}

	if err := vm.addr().Valid(); err != nil {
		return err
	}

	keys := make([]string, 0, len(svc.Env))
	for k := range svc.Env {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	env := fc.MergeEnv(rfs.Config.Env, svc.Env, keys)
	// execd reads both and removes them from its own environment before it starts anything,
	// so the workload never inherits either.
	env = append(env, "EXECD_ACCESS_TOKEN="+vm.AccessToken, "EXECD_CONTROL_SECRET="+vm.ControlSecret)

	init := fc.InitConfig{
		Argv:       fc.Compose(rfs.Config.Entrypoint, rfs.Config.Cmd, svc.Entrypoint, svc.Args),
		Env:        env,
		WorkingDir: rfs.Config.WorkingDir,
		Hostname:   service,
		RootDevice: fc.GuestRootfsDevice,
	}

	if err := fc.BuildAgentDrive(ctx, p.ext4, fc.AgentDrive{Agent: agent, Config: init},
		filepath.Join(dir, "agent.ext4")); err != nil {
		return err
	}

	if err := p.save(vm); err != nil {
		return err
	}

	if err := p.coldBoot(ctx, vm); err != nil {
		return err
	}

	bctx, cancel := context.WithTimeout(ctx, p.bootTimeout)
	defer cancel()

	if err := p.ready(bctx, vm, dir); err != nil {
		return fmt.Errorf("%s booted but never served: %w\n%s", ref, err, p.consoleTail(dir, 20))
	}

	if err := p.sleep(ctx, vm); err != nil {
		return err
	}

	ok = true

	return nil
}

// coldBoot starts a VM from its drives, as after a power-on.
func (p *fcProvider) coldBoot(ctx context.Context, vm *fcVM) error {
	dir := p.dir(vm.Ref)
	a := vm.addr()

	if err := p.net.EnsureTap(ctx, a); err != nil {
		return err
	}

	if _, err := p.launch.Launch(ctx, fc.LaunchSpec{Binary: vm.Binary, Dir: dir, ID: vm.Instance}); err != nil {
		return err
	}

	c := p.client(vm.Ref)

	// panic=1 reboot=k: a guest that panics or whose PID 1 exits takes the VMM down with it,
	// which the provider reads as asleep-without-a-snapshot and cold-boots next time.
	args := "console=ttyS0 reboot=k panic=1 quiet loglevel=3 init=/sbx " + a.BootArg() + " -- fc-init"

	for _, step := range []func() error{
		func() error { return c.PutBootSource(ctx, fc.BootSource{KernelImagePath: vm.Kernel, BootArgs: args}) },
		func() error {
			return c.PutDrive(ctx, fc.Drive{DriveID: "agent", PathOnHost: filepath.Join(dir, "agent.ext4"), IsRootDevice: true, IsReadOnly: true})
		},
		func() error {
			return c.PutDrive(ctx, fc.Drive{DriveID: "rootfs", PathOnHost: filepath.Join(dir, fc.RootfsName)})
		},
		func() error {
			return c.PutMachineConfig(ctx, fc.MachineConfig{VcpuCount: vm.VCPU, MemSizeMib: vm.MemMiB, TrackDirtyPages: true})
		},
		func() error {
			return c.PutNetworkInterface(ctx, fc.NetworkInterface{IfaceID: "eth0", HostDevName: a.Tap(), GuestMAC: a.MAC()})
		},
		func() error {
			return c.PutVsock(ctx, fc.Vsock{GuestCID: 3, UDSPath: filepath.Join(dir, fc.VsockName)})
		},
		func() error { return c.PutEntropy(ctx) },
		func() error { return c.InstanceStart(ctx) },
	} {
		if err := step(); err != nil {
			_ = p.launch.Kill(context.WithoutCancel(ctx), dir)
			return err
		}
	}

	// A fresh execd from the agent drive: it holds the boot secret again.
	vm.Restored = false
	vm.LiveSecret = ""

	return nil
}

// sleep snapshots a running VM and ends its process. The caller holds the lock.
func (p *fcProvider) sleep(ctx context.Context, vm *fcVM) error {
	dir := p.dir(vm.Ref)
	c := p.client(vm.Ref)

	if p.guest.Available() {
		if err := p.guest.Seal(ctx, p.guestVM(vm), vm.secret()); err != nil {
			return fmt.Errorf("sealing execd before the snapshot: %w - the VM is still running", err)
		}
	}

	if err := c.Pause(ctx); err != nil {
		return err
	}

	state := filepath.Join(dir, fc.StateName)

	if vm.Restored {
		// A Diff over the base this process was loaded from: only what it dirtied.
		diff := filepath.Join(dir, fc.DiffMemName)

		if err := c.CreateSnapshot(ctx, fc.SnapshotCreate{SnapshotType: fc.SnapshotDiff,
			SnapshotPath: state + ".new", MemFilePath: diff}); err != nil {
			return err
		}

		if err := p.launch.Kill(ctx, dir); err != nil {
			return err
		}

		if err := p.merge(diff, filepath.Join(dir, fc.MemName)); err != nil {
			return err
		}

		_ = os.Remove(diff)
	} else {
		mem := filepath.Join(dir, fc.MemName)

		if err := c.CreateSnapshot(ctx, fc.SnapshotCreate{SnapshotType: fc.SnapshotFull,
			SnapshotPath: state + ".new", MemFilePath: mem + ".new"}); err != nil {
			return err
		}

		if err := p.launch.Kill(ctx, dir); err != nil {
			return err
		}

		if err := os.Rename(mem+".new", mem); err != nil {
			return err
		}
	}

	if err := os.Rename(state+".new", state); err != nil {
		return err
	}

	vm.SnapshotValid = true
	vm.Restored = false

	return p.save(vm)
}

// running asks the VMM what it is doing. Unreachable is "" - asleep - not an error.
func (p *fcProvider) running(ctx context.Context, ref string) string {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	info, err := p.client(ref).Describe(ctx)
	if err != nil {
		return ""
	}

	return info.State
}

func (p *fcProvider) Start(ctx context.Context, ref string) error {
	vm, err := p.load(ref)
	if err != nil {
		return err
	}

	unlock := p.lock(ref)
	defer unlock()

	switch p.running(ctx, ref) {
	case fc.StateRunning:
		return nil
	case fc.StatePaused:
		return p.client(ref).Resume(ctx)
	}

	dir := p.dir(ref)

	// A VMM that is up but not started, or a stale process: clear it before a new one binds.
	_ = p.launch.Kill(ctx, dir)

	if !vm.SnapshotValid {
		fmt.Fprintf(os.Stderr, "  %s: cold boot - its last snapshot no longer matches its disk "+
			"(it stopped without being slept)\n", ref)

		if err := p.coldBoot(ctx, vm); err != nil {
			return err
		}

		return p.save(vm)
	}

	return p.restore(ctx, vm)
}

// restore loads the snapshot and resumes it. The caller holds the lock.
func (p *fcProvider) restore(ctx context.Context, vm *fcVM) error {
	dir := p.dir(vm.Ref)
	a := vm.addr()

	if err := p.net.EnsureTap(ctx, a); err != nil {
		return err
	}

	// Invalid BEFORE the resume, durably - see the file comment.
	vm.SnapshotValid = false
	if err := p.save(vm); err != nil {
		return err
	}

	if _, err := p.launch.Launch(ctx, fc.LaunchSpec{Binary: vm.Binary, Dir: dir, ID: vm.Instance}); err != nil {
		return p.revalidate(vm, err)
	}

	err := p.client(vm.Ref).LoadSnapshot(ctx, fc.SnapshotLoad{
		SnapshotPath:     filepath.Join(dir, fc.StateName),
		MemBackend:       fc.MemBackend{BackendType: "File", BackendPath: filepath.Join(dir, fc.MemName)},
		TrackDirtyPages:  true,
		ResumeVM:         true,
		NetworkOverrides: []fc.NetworkOverride{{IfaceID: "eth0", HostDevName: a.Tap()}},
		VsockOverride:    &fc.VsockOverride{UDSPath: filepath.Join(dir, fc.VsockName)},
	})
	if err != nil {
		_ = p.launch.Kill(context.WithoutCancel(ctx), dir)

		// A load that failed never ran the guest, so the disk did not move and the snapshot
		// is still good.
		return p.revalidate(vm, err)
	}

	vm.Restored = true
	vm.Generation++

	if p.guest.Available() {
		// Before returning, so the wake proxy never passes a request to an execd that has not
		// been given its identity. A failure kills the VM rather than serve sealed or stale.
		// The control secret rotates: execd refuses a re-key that keeps the one the snapshot
		// holds, since every clone of that snapshot holds it too.
		next := randomHex(32)

		if err := p.guest.Rekey(ctx, p.guestVM(vm), fc.Rekey{
			Secret: vm.secret(), Generation: vm.Generation, AccessToken: vm.AccessToken, ControlSecret: next,
		}); err != nil {
			_ = p.launch.Kill(context.WithoutCancel(ctx), dir)
			_ = p.save(vm)

			return fmt.Errorf("re-keying execd after the restore: %w - the VM was stopped rather "+
				"than left serving with the snapshot's identity", err)
		}

		vm.LiveSecret = next
	}

	if err := p.save(vm); err != nil {
		// execd now holds a secret this record does not: left running, it could never be
		// sealed again. Stopped, its next wake is a cold boot with the boot secret.
		_ = p.launch.Kill(context.WithoutCancel(ctx), dir)

		return fmt.Errorf("recording the restored VM: %w - it was stopped", err)
	}

	return nil
}

func (p *fcProvider) revalidate(vm *fcVM, cause error) error {
	vm.SnapshotValid = true
	if err := p.save(vm); err != nil {
		return errors.Join(cause, err)
	}

	return cause
}

func (p *fcProvider) Stop(ctx context.Context, ref string) error {
	vm, err := p.load(ref)
	if err != nil {
		return err
	}

	unlock := p.lock(ref)
	defer unlock()

	switch p.running(ctx, ref) {
	case "":
		// Already asleep - or died without a snapshot, which Start will find and cold-boot.
		return p.launch.Kill(ctx, p.dir(ref))
	case fc.StateNotStarted:
		return p.launch.Kill(ctx, p.dir(ref))
	}

	return p.sleep(ctx, vm)
}

// Pause and Unpause freeze the vCPUs in place: memory stays resident, no CPU is used. The
// on_idle: freeze path, and ~3 ms back to the first byte on the spike's nested setup.
func (p *fcProvider) Pause(ctx context.Context, ref string) error {
	unlock := p.lock(ref)
	defer unlock()

	switch p.running(ctx, ref) {
	case fc.StatePaused:
		return nil
	case fc.StateRunning:
		return p.client(ref).Pause(ctx)
	default:
		return fmt.Errorf("%s is not running, so there is nothing to freeze", ref)
	}
}

func (p *fcProvider) Unpause(ctx context.Context, ref string) error {
	unlock := p.lock(ref)
	defer unlock()

	if p.running(ctx, ref) == fc.StatePaused {
		return p.client(ref).Resume(ctx)
	}

	return nil
}

// Healthy and Probe dial the guest's first port. declared is false on purpose: a spec's health
// command runs inside the workload, which needs the guest agent, so readiness here is an accepted
// connection - and the interface says to report that as a guess rather than dress it as a check.
func (p *fcProvider) Healthy(ctx context.Context, ref string) (bool, bool) { return p.Probe(ctx, ref) }

func (p *fcProvider) Probe(ctx context.Context, ref string) (bool, bool) {
	vm, err := p.load(ref)
	if err != nil || len(vm.Ports) == 0 || p.running(ctx, ref) != fc.StateRunning {
		return false, false
	}

	var d net.Dialer

	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(vm.addr().GuestIP(), strconv.Itoa(vm.Ports[0])))
	if err != nil {
		return false, false
	}

	_ = c.Close()

	return true, false
}

// waitServing is Create's readiness: the first port accepting, or for a service with none, execd
// having said something on the console.
func (p *fcProvider) waitServing(ctx context.Context, vm *fcVM, dir string) error {
	for {
		if len(vm.Ports) > 0 {
			var d net.Dialer

			dctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			c, err := d.DialContext(dctx, "tcp", net.JoinHostPort(vm.addr().GuestIP(), strconv.Itoa(vm.Ports[0])))
			cancel()

			if err == nil {
				_ = c.Close()
				return nil
			}
		} else if b, _ := os.ReadFile(filepath.Join(dir, fc.ConsoleName)); strings.Contains(string(b), "sbx execd:") {
			return nil
		}

		if p.running(ctx, vm.Ref) == "" {
			return errors.New("the VM exited during boot")
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("nothing answered on %s within the boot timeout (SBX_FC_BOOT_TIMEOUT)",
				vm.addr().GuestIP())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (p *fcProvider) consoleTail(dir string, n int) string {
	var buf strings.Builder
	_ = tailFile(filepath.Join(dir, fc.ConsoleName), n, &buf)

	return "the guest console's last lines:\n" + buf.String()
}

func (p *fcProvider) all() ([]*fcVM, error) {
	ents, err := os.ReadDir(filepath.Join(p.root, "vms"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var out []*fcVM

	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(p.root, "vms", e.Name(), "vm.json"))
		if err != nil {
			continue // a directory mid-create or mid-remove
		}

		var vm fcVM
		if json.Unmarshal(b, &vm) == nil {
			out = append(out, &vm)
		}
	}

	return out, nil
}

func (p *fcProvider) List(ctx context.Context, sandbox string) ([]Unit, error) {
	vms, err := p.all()
	if err != nil {
		return nil, err
	}

	var units []Unit

	for _, vm := range vms {
		if sandbox != "" && vm.Sandbox != sandbox {
			continue
		}

		state := p.running(ctx, vm.Ref)

		u := Unit{
			Sandbox: vm.Sandbox, Service: vm.Service, Slot: vm.Slot, Ref: vm.Ref,
			Instance: vm.Instance, Running: state == fc.StateRunning, Paused: state == fc.StatePaused,
			Index: vm.Index % blockSize, DependsOn: vm.DependsOn, Idle: vm.Idle, OnIdle: vm.OnIdle,
		}

		for i, pub := range vm.Public {
			u.Client = append(u.Client, Endpoint{Host: "127.0.0.1", Port: pub})
			u.Listen = append(u.Listen, pub)
			u.Upstream = append(u.Upstream, Endpoint{Host: vm.addr().GuestIP(), Port: vm.Ports[i]})
		}

		units = append(units, u)
	}

	sort.Slice(units, func(i, j int) bool {
		if units[i].Sandbox != units[j].Sandbox {
			return units[i].Sandbox < units[j].Sandbox
		}

		return units[i].Service < units[j].Service
	})

	return units, nil
}

func (p *fcProvider) Remove(ctx context.Context, sandbox string) error {
	vms, err := p.all()
	if err != nil {
		return err
	}

	slot, found := -1, false

	for _, vm := range vms {
		if vm.Sandbox != sandbox {
			continue
		}

		found, slot = true, vm.Slot

		unlock := p.lock(vm.Ref)
		dir := p.dir(vm.Ref)

		if err := p.launch.Kill(ctx, dir); err != nil {
			unlock()
			return err
		}

		if err := p.net.RemoveTap(ctx, vm.addr()); err != nil {
			unlock()
			return err
		}

		err := os.RemoveAll(dir)

		unlock()

		if err != nil {
			return err
		}

		fmt.Printf("  removed %s (vm, snapshot and root filesystem)\n", vm.Ref)
	}

	if !found {
		return fmt.Errorf("no sandbox %q", sandbox)
	}

	return p.net.RemoveBridge(ctx, slot)
}

// errNoGuest is every operation that needs the guest agent before it is wired.
func errNoGuest(op string) error {
	return fmt.Errorf("the firecracker provider cannot %s yet: %w", op, fc.ErrGuestUnavailable)
}

// Logs is the serial console: the kernel, fc-init, execd and the workload, in one stream,
// across every sleep and wake the VM has had.
func (p *fcProvider) Logs(ctx context.Context, ref string, lines int, follow bool, w io.Writer) error {
	path := filepath.Join(p.dir(ref), fc.ConsoleName)

	if _, err := p.load(ref); err != nil {
		return err
	}

	if err := tailFile(path, lines, w); err != nil {
		return err
	}

	if !follow {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	for {
		if _, err := io.Copy(w, f); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func tailFile(path string, n int, w io.Writer) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}
	defer f.Close()

	var ring []string

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)

	for sc.Scan() {
		ring = append(ring, sc.Text())
		if n > 0 && len(ring) > n {
			ring = ring[1:]
		}
	}

	for _, l := range ring {
		if _, err := fmt.Fprintln(w, l); err != nil {
			return err
		}
	}

	return sc.Err()
}

// Limits implements Limiter: what the VM was booted with.
func (p *fcProvider) Limits(_ context.Context, ref string) (Limits, error) {
	vm, err := p.load(ref)
	if err != nil {
		return Limits{}, err
	}

	return Limits{NanoCPUs: int64(vm.VCPU) * 1e9, MemBytes: uint64(vm.MemMiB) << 20}, nil
}

// SetLimits is refused: vCPUs and memory are fixed when a Firecracker VM boots, and its
// snapshot carries them, so a sleeping VM cannot be resized either. (Balloon and memory hotplug
// exist in v1.17.0; neither changes what the guest was booted believing it has.)
func (p *fcProvider) SetLimits(_ context.Context, ref string, _ Limits) error {
	return fmt.Errorf("%s is a Firecracker VM: its vCPUs and memory are fixed at boot and saved in "+
		"its snapshot. Change cpu/memory in the spec and recreate the service", ref)
}

// Snapshotter. A firecracker snapshot is the VM - memory, running processes and the root
// filesystem together - which is the one thing `sbx snapshot` cannot give on docker. It is kept
// under <state>/snapshots/<image>/, and restoring it is creating a service whose image names it.

func (p *fcProvider) snapshotDir(image string) string {
	return filepath.Join(p.root, "snapshots", strings.NewReplacer("/", "_", ":", "_").Replace(image))
}

type fcSnapshot struct {
	VM  fcVM   `json:"vm"`
	Dir string `json:"dir"` // the VM directory it was taken in: drive paths in vm.state point there
}

func (p *fcProvider) snapshotFor(image string) (*fcSnapshot, bool) {
	b, err := os.ReadFile(filepath.Join(p.snapshotDir(image), "snapshot.json"))
	if err != nil {
		return nil, false
	}

	var s fcSnapshot
	if json.Unmarshal(b, &s) != nil {
		return nil, false
	}

	return &s, true
}

// Commit saves a running or sleeping VM - memory included - under image.
func (p *fcProvider) Commit(ctx context.Context, ref, image string, changes ...string) error {
	if len(changes) > 0 {
		return fmt.Errorf("a firecracker snapshot is a memory image; it cannot take image-config "+
			"changes (%s) the way docker commit does", strings.Join(changes, ", "))
	}

	vm, err := p.load(ref)
	if err != nil {
		return err
	}

	unlock := p.lock(ref)
	defer unlock()

	dst := p.snapshotDir(image)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}

	dir := p.dir(ref)

	copyVM := func(withMemory bool) error {
		for _, f := range []string{fc.RootfsName, "agent.ext4"} {
			if _, err := fc.CloneFile(filepath.Join(dir, f), filepath.Join(dst, f)); err != nil {
				return err
			}
		}

		if !withMemory {
			return nil
		}

		for _, f := range []string{fc.StateName, fc.MemName} {
			if _, err := fc.CloneFile(filepath.Join(dir, f), filepath.Join(dst, f)); err != nil {
				return err
			}
		}

		return nil
	}

	switch p.running(ctx, ref) {
	case fc.StateRunning, fc.StatePaused:
		c := p.client(ref)

		if err := c.Pause(ctx); err != nil {
			return err
		}

		err := c.CreateSnapshot(ctx, fc.SnapshotCreate{SnapshotType: fc.SnapshotFull,
			SnapshotPath: filepath.Join(dst, fc.StateName), MemFilePath: filepath.Join(dst, fc.MemName)})
		if err == nil {
			err = copyVM(false)
		}

		// A snapshot resets Firecracker's dirty bitmap; the next sleep must be Full.
		vm.Restored = false
		_ = p.save(vm)

		if rerr := c.Resume(ctx); err == nil {
			err = rerr
		}

		if err != nil {
			return err
		}
	default:
		if !vm.SnapshotValid {
			return fmt.Errorf("%s has no valid snapshot to save (it stopped without being slept); "+
				"start it and snapshot it while it runs", ref)
		}

		if err := copyVM(true); err != nil {
			return err
		}
	}

	if err := os.WriteFile(filepath.Join(dst, "name"), []byte(image+"\n"), 0o600); err != nil {
		return err
	}

	b, err := json.Marshal(fcSnapshot{VM: *vm, Dir: dir})
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dst, "snapshot.json"), b, 0o600)
}

// createFromSnapshot restores a saved VM as a new service - only where its paths and address
// still hold. A Firecracker snapshot bakes in the host paths of its drives and the guest's IP
// (configured by the kernel at the original boot), so it can come back as the same sandbox and
// service in the same slot, and nowhere else, until drives are re-pointed per clone (a jailer or
// a mount namespace) and the guest agent re-addresses the network. A clone under another name
// is also a fork, which needs the guest to re-key - refused for both reasons, each stated.
func (p *fcProvider) createFromSnapshot(_ context.Context, s *fcSnapshot, image, ref string, slot int, eps []Endpoint) error {
	dir := p.dir(ref)

	switch {
	case s.Dir != dir:
		return fmt.Errorf("the firecracker snapshot for %s/%s can only be restored as that same "+
			"sandbox and service: its drive paths are baked into the VM state, and restoring it "+
			"under another name would be a fork, which needs the guest agent to re-key it", s.VM.Sandbox, s.VM.Service)
	case s.VM.Slot != slot:
		return fmt.Errorf("the firecracker snapshot for %s was taken in slot %d, and this sandbox "+
			"got slot %d: the guest's address is part of its memory. Free slot %d and retry",
			ref, s.VM.Slot, slot, s.VM.Slot)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	unlock := p.lock(ref)
	defer unlock()

	src := p.snapshotDir(image)

	for _, f := range []string{fc.RootfsName, "agent.ext4", fc.StateName, fc.MemName} {
		if _, err := fc.CloneFile(filepath.Join(src, f), filepath.Join(dir, f)); err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("restoring %s from its snapshot: %w", f, err)
		}
	}

	vm := s.VM
	vm.Instance = randomHex(8)
	vm.Created = time.Now().UTC()
	vm.SnapshotValid = true
	vm.Restored = false
	vm.Public = nil

	for _, e := range eps {
		vm.Public = append(vm.Public, e.Port)
	}

	// Born asleep, like a fresh create: the first connection restores it.
	return p.save(&vm)
}

// Images lists saved VM snapshots whose name begins with prefix.
func (p *fcProvider) Images(_ context.Context, prefix string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(p.root, "snapshots"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var out []string

	for _, e := range ents {
		s, ok := p.snapshotByDir(filepath.Join(p.root, "snapshots", e.Name()))
		if ok && strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}

	slices.Sort(out)

	return out, nil
}

func (p *fcProvider) snapshotByDir(dir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "name"))
	if err != nil {
		return "", false
	}

	return strings.TrimSpace(string(b)), true
}

func (p *fcProvider) RemoveImage(_ context.Context, image string) error {
	return os.RemoveAll(p.snapshotDir(image))
}

// VolumeFor is empty: a VM's data is its root filesystem, which Commit already saves with the
// memory. There is no separate volume for the CLI to copy.
func (p *fcProvider) VolumeFor(string, string) string { return "" }

func (p *fcProvider) CopyVolume(context.Context, string, string) error {
	return errors.New("a firecracker service has no separate data volume; its snapshot carries its disk")
}

// The capabilities this provider has natively, checked at compile time so that dropping one is
// a build failure rather than a CLI that quietly starts refusing.
var (
	_ Provider    = (*fcProvider)(nil)
	_ Snapshotter = (*fcProvider)(nil)
	_ Pauser      = (*fcProvider)(nil)
	_ Limiter     = (*fcProvider)(nil)
)
