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

// DecideHost is the ONE decision about where a microVM runs from this machine. The helper-VM
// layer (fchost.HostBackend: hostcap's Linux and macOS verdict, plus the SBX_FC_ASSUME_NESTED
// override, the VM tool that would run the helper VM, and the whole Windows branch) installs it
// from its init, so the provider, the CLI redirect, `sbx serve` and `sbx doctor` all act on the
// same answer. The default - hostcap alone - is only what a build without that layer sees.
var DecideHost = func() hostcap.Decision { return hostcap.Decide(probeHost()) }

// Version is the sbx build version, used to find the agent binary; app sets it.
var Version = "dev"

func forFirecracker(socket string) (Provider, error) {
	d := DecideHost()

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
	Health    string    `json:"health,omitempty"` // the spec's health command, run by execd
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

	// PendingSecret is a re-key's new secret, recorded BEFORE the re-key is sent: a process that
	// dies between execd accepting it and this record saying so leaves execd holding a secret the
	// record would otherwise not know. Seal tries it when the live one is refused.
	PendingSecret string `json:"pending_secret,omitempty"`

	// SnapshotValid: vm.state + vm.mem describe the disk as it is now. See the file comment.
	SnapshotValid bool `json:"snapshot_valid"`

	// Restored: the running process was loaded from vm.mem with dirty tracking, so a Diff on
	// top of vm.mem is a correct snapshot. False after a cold boot, and after a Commit, which
	// resets Firecracker's dirty bitmap and would make the next Diff miss pages.
	Restored bool `json:"restored"`

	// EgressPolicy is the egress policy the spec declared, as JSON, when the service reaches the
	// network through the filter the daemon serves on this sandbox's bridge gateway; "" when it
	// has no way out at all. What the filter STARTS with and what a reset returns to: a policy
	// changed on the running VM lives with the filter (DECISIONS.md, "A live egress policy is
	// held by the filter and pushed to it").
	EgressPolicy string `json:"egress_policy,omitempty"`
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

	// probeDial reaches a guest port for Probe; nil is a plain TCP dial. A field for tests.
	probeDial func(ctx context.Context, addr string) (net.Conn, error)

	// healthExec runs a service's health command in its VM; p.Exec (execd over vsock), a field
	// for tests.
	healthExec func(ctx context.Context, ref string, argv []string) (string, error)

	// bridgeCheck reads whether this host drops traffic between sandbox bridges
	// (fc.HostBridgeIsolation); nil skips it. warn is where Create says so; nil is stderr.
	bridgeCheck func() fc.BridgeIsolation
	warn        io.Writer

	// boots caps how many VMs restore or cold-boot at once: each is a burst of page faults and a
	// vCPU spinning up, and a fleet woken together (a host reboot, a burst of connections) would
	// otherwise contend so hard that every wake is slow. Sized to the host's CPUs; nil is no cap.
	boots chan struct{}

	mu    sync.Mutex
	locks map[string]*refLock
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
		bridgeCheck: fc.HostBridgeIsolation,
		boots:       make(chan struct{}, max(1, runtime.NumCPU())),
		bootTimeout: 60 * time.Second,
		locks:       map[string]*refLock{},
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

// refLock is one ref's mutex and how many callers hold or wait for it. Counted so the entry goes
// when the last one leaves: a long-lived daemon that creates and removes services all day must not
// keep a mutex for every name it ever saw.
type refLock struct {
	sync.Mutex
	n int
}

func (p *fcProvider) lock(ref string) func() {
	p.mu.Lock()

	l, ok := p.locks[ref]
	if !ok {
		l = &refLock{}
		p.locks[ref] = l
	}

	l.n++

	p.mu.Unlock()
	l.Lock()

	// And across processes: `sbx serve` sleeping a VM while `sbx rm` removes it.
	unlock := fileLock(filepath.Join(p.dir(ref), fc.LockFileName))

	return func() {
		unlock()
		l.Unlock()

		p.mu.Lock()
		if l.n--; l.n == 0 {
			delete(p.locks, ref)
		}
		p.mu.Unlock()
	}
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

	// Slots whose ports the machine in front of this one already holds (the helper-VM case).
	for s := range parseSlots(os.Getenv(HostBusySlotsEnv)) {
		used[s] = true
	}

	for i := range maxSlots {
		if used[i] || !p.portsFree(i) {
			continue
		}

		return i, nil
	}

	return 0, fmt.Errorf("all %d sandbox slots are in use; destroy one first", maxSlots)
}

// HostBusySlotsEnv lists, comma-separated, the slots whose public ports the HOST holds when this
// provider runs in a helper VM. The Mac mirrors every sandbox port at the same number on its own
// loopback, where a docker sandbox of the user's may already be; probing the VM's loopback
// cannot see that, so the host probes and says (fchost.GuestArgv).
const HostBusySlotsEnv = "SBX_FC_HOST_BUSY_SLOTS"

func parseSlots(s string) map[int]bool {
	out := map[int]bool{}

	for _, f := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n >= 0 && n < maxSlots {
			out[n] = true
		}
	}

	return out
}

// SlotPortsFree reports whether a slot's public ports can be bound on this machine.
func SlotPortsFree(slot int) bool { return publicPortsFree(slot) }

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
	add(len(svc.Files) > 0, "files", "not written into a VM yet: execd can copy into a running VM, but create "+
		"does not do it before the first snapshot")
	add(len(svc.Mounts) > 0 || len(svc.VolumeMounts) > 0 || len(svc.ReadOnlyVolumes) > 0,
		"mounts", "a host directory cannot be bind-mounted into a VM; it would be a virtio-fs device")
	add(len(svc.Init) > 0, "init", "not run in a VM yet: execd can run commands, but create does not run them "+
		"after the first healthy check")
	add(svc.GPUs != "", "gpus", "Firecracker has no device passthrough")
	add(len(svc.CapAdd) > 0, "cap_add", "the workload is root in its own kernel; there is no capability set to widen")
	add(svc.Egress != "" && svc.Egress != spec.EgressDeny && svc.Egress != spec.EgressAllow, "egress",
		"a VM bridge has no NAT: its egress is deny, or the filter (allow, egress_allow, egress_policy)")

	if len(why) == 0 {
		return nil
	}

	return fmt.Errorf("the firecracker provider cannot honour %s - remove them, or use --provider docker",
		strings.Join(why, "; "))
}

// rootUser reports whether an image's USER is root: unset, or root by name or uid, with or without
// a root group.
func rootUser(u string) bool {
	user, group, _ := strings.Cut(u, ":")

	return (user == "" || user == "root" || user == "0") && (group == "" || group == "root" || group == "0")
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

	// health runs inside the VM through execd; with no guest agent there is nothing to run it,
	// and a health command that is silently never run is the one thing the spec forbids.
	if svc.Health != "" && !p.guest.Available() {
		return fmt.Errorf("the firecracker provider cannot honour health (%q): it runs inside the VM "+
			"through the guest agent, which this build does not have - remove it, or use --provider docker", svc.Health)
	}

	vcpu, mem, err := sizing(svc)
	if err != nil {
		return err
	}

	// Said on every create, loudly: sbx writes no firewall rule, so a host that routes between
	// bridges lets this sandbox's VMs reach every other sandbox's.
	if p.bridgeCheck != nil {
		if iso := p.bridgeCheck(); iso.Known && !iso.Isolated {
			w := p.warn
			if w == nil {
				w = os.Stderr
			}

			fmt.Fprintf(w, "  warning: microVM sandboxes may not be isolated from each other: %s - %s\n", iso.Detail, iso.Meaning)
		}
	}

	ref := containerName(sandbox, service)
	dir := p.dir(ref)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// The existence check is made holding the lock, and before the cleanup below is armed: two
	// concurrent creates of one service must not both pass it, and the one that loses must not
	// RemoveAll the directory the winner just filled.
	unlock := p.lock(ref)
	defer unlock()

	if _, err := os.Stat(filepath.Join(dir, "vm.json")); err == nil {
		return fmt.Errorf("%s already exists; sbx rm %s first", ref, sandbox)
	}

	// A failure from here on leaves nothing behind: a half-made VM directory would be listed
	// as a service that can never start.
	ok := false

	var made *fcVM // once set, coldBoot may have made its tap and bridge

	defer func() {
		if ok {
			return
		}

		ctx := context.WithoutCancel(ctx)
		_ = p.launch.Kill(ctx, dir)
		_ = os.RemoveAll(dir)

		if made != nil {
			p.releaseNet(ctx, made)
		}
	}()

	if snap, found := p.snapshotFor(svc.Image); found {
		if err := p.createFromSnapshot(ctx, snap, svc, ref, slot, eps); err != nil {
			return err
		}

		ok = true

		return nil
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

	if !rootUser(rfs.Config.User) {
		return fmt.Errorf("%s runs as USER %q, and the firecracker provider runs every process in the VM "+
			"as root: fc-init and execd do not switch users yet, and running it as root anyway would "+
			"quietly drop the boundary the image asked for - use --provider docker, or an image whose "+
			"USER is root", svc.Image, rfs.Config.User)
	}

	agent, err := p.agent(ctx, p.arch)
	if err != nil {
		return err
	}

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
		VCPU: vcpu, MemMiB: mem, Ports: svc.Ports, DependsOn: svc.DependsOn, Health: svc.Health,
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

	made = vm

	keys := make([]string, 0, len(svc.Env))
	for k := range svc.Env {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	env := fc.MergeEnv(rfs.Config.Env, svc.Env, keys)
	// execd reads both and removes them from its own environment before it starts anything,
	// so the workload does not inherit either. That keeps them out of `env` and logs; it does
	// not hide them from root in the guest, which can read /proc/1/environ and /init.json on
	// the agent drive. Harmless by construction (SECURITY.md): each is this guest's own, control
	// is reachable only over vsock from the host, the control secret rotates at every restore,
	// and a fork of a VM is refused.
	env = append(env, "EXECD_ACCESS_TOKEN="+vm.AccessToken, "EXECD_CONTROL_SECRET="+vm.ControlSecret)

	if svc.Filtered() {
		if vm.EgressPolicy, err = declaredJSON(svc); err != nil {
			return err
		}

		env = withEgressProxy(env, vm.addr())
	}

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

	release, err := p.bootSlot(ctx)
	if err != nil {
		return err
	}

	defer release() // safe twice: released early below once the VM serves

	if err := p.coldBoot(ctx, vm); err != nil {
		return err
	}

	bctx, cancel := context.WithTimeout(ctx, p.bootTimeout)
	defer cancel()

	if err := p.ready(bctx, vm, dir); err != nil {
		return fmt.Errorf("%s booted but never served: %w\n%s", ref, err, p.consoleTail(dir, 20))
	}

	// The snapshot every wake restores must be of a workload that SERVES, not one whose port is
	// merely open: its health command passes here, once, so no wake has to run it again.
	if svc.Health != "" {
		if err := p.waitHealth(bctx, ref, svc.Health); err != nil {
			return fmt.Errorf("%s booted but never became healthy: %w\n%s", ref, err, p.consoleTail(dir, 20))
		}
	}

	release()

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

	if err := p.awaitPrevious(ctx, vm.Ref); err != nil {
		return err
	}

	clearVsock(dir)

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
	vm.LiveSecret, vm.PendingSecret = "", ""

	return nil
}

// sealTimeout bounds execd's answer to Seal. A guest that stalls it is vetoing its own sleep.
const sealTimeout = 10 * time.Second

// sleep snapshots a running VM and ends its process. The caller holds the lock.
//
// Whatever fails, the VM does not stay up. After a Seal execd answers nobody until re-keyed, and a
// Start that finds the process running (or paused) only resumes it, so a VM left behind by a
// failed sleep would be up and permanently deaf. And a Seal that fails or times out is the guest
// refusing to be slept - PID 1 is the workload's to stall - which must not let it pin host
// memory. So a failure kills the VMM: the record already says SnapshotValid=false for a running
// VM, so the next wake cold-boots it from its disk. Memory is lost; the disk and the service are
// not.
func (p *fcProvider) sleep(ctx context.Context, vm *fcVM) error {
	err := p.snapshotAndEnd(ctx, vm)
	if err == nil {
		return nil
	}

	return errors.Join(fmt.Errorf("sleeping %s: %w - its VM was stopped without a usable snapshot, and "+
		"its next wake is a cold boot", vm.Ref, err), p.abandon(ctx, vm))
}

// seal asks execd to seal, with the secret it holds: the recorded one, or - when that is refused
// and a re-key's answer was never recorded - the pending one. Bounded: a guest that stalls it is
// vetoing its own sleep.
func (p *fcProvider) seal(ctx context.Context, vm *fcVM) error {
	ctx, cancel := context.WithTimeout(ctx, sealTimeout)
	defer cancel()

	err := p.guest.Seal(ctx, p.guestVM(vm), vm.secret())
	if err == nil || vm.PendingSecret == "" || vm.PendingSecret == vm.secret() {
		return err
	}

	if perr := p.guest.Seal(ctx, p.guestVM(vm), vm.PendingSecret); perr != nil {
		return errors.Join(err, perr)
	}

	vm.LiveSecret, vm.PendingSecret = vm.PendingSecret, ""

	return nil
}

// rekey gives execd a fresh control secret at vm.Generation, recording it as pending first.
func (p *fcProvider) rekey(ctx context.Context, vm *fcVM) error {
	next := randomHex(32)

	vm.PendingSecret = next
	if err := p.save(vm); err != nil {
		return err
	}

	if err := p.guest.Rekey(ctx, p.guestVM(vm), fc.Rekey{
		Secret: vm.secret(), Generation: vm.Generation, AccessToken: vm.AccessToken, ControlSecret: next,
	}); err != nil {
		return err
	}

	vm.LiveSecret, vm.PendingSecret = next, ""

	return nil
}

// abandon kills a running VM whose execd can no longer be trusted to answer, and records that its
// next wake is a cold boot from its disk. The caller holds the lock.
func (p *fcProvider) abandon(ctx context.Context, vm *fcVM) error {
	kerr := p.launch.Kill(context.WithoutCancel(ctx), p.dir(vm.Ref))

	vm.SnapshotValid, vm.Restored, vm.LiveSecret, vm.PendingSecret = false, false, "", ""

	return errors.Join(kerr, p.save(vm))
}

func (p *fcProvider) snapshotAndEnd(ctx context.Context, vm *fcVM) error {
	dir := p.dir(vm.Ref)
	c := p.client(vm.Ref)

	if p.guest.Available() {
		if err := p.seal(ctx, vm); err != nil {
			return fmt.Errorf("sealing execd before the snapshot: %w", err)
		}
	}

	if err := c.Pause(ctx); err != nil {
		return err
	}

	state := filepath.Join(dir, fc.StateName)

	if vm.Restored {
		// A Diff over the base this process was loaded from: only what it dirtied.
		diff := filepath.Join(dir, fc.DiffMemName)

		// Never write into a file that is already there: a diff.mem left by an earlier failed
		// merge carries stale pages the merge would fold in as if this sleep had dirtied them, and
		// anything at that name - a symlink included - is not this snapshot's to follow.
		if err := os.Remove(diff); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

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

// describeTimeout bounds one "what are you doing" to the VMM.
const describeTimeout = 500 * time.Millisecond

// running asks the VMM what it is doing. No process is "" - asleep - and not an error. A process
// that is alive and did not answer in time is an error: it is a VM under load, or a VMM stuck in
// the kernel, and treating it as asleep is what made Stop and Start kill a live VM.
func (p *fcProvider) running(ctx context.Context, ref string) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()

	info, err := p.client(ref).Describe(dctx)

	switch {
	case err == nil:
		return info.State, nil
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.Is(err, fc.ErrUnreachable):
		// Nothing is serving the API: gone, or a killed VMM still closing its files. Not running
		// either way (Start waits for the files - awaitPrevious - before it launches another).
		return "", nil
	case !p.launch.Alive(p.dir(ref)):
		return "", nil
	default:
		return "", fmt.Errorf("the firecracker process for %s is alive but did not answer its API within %s "+
			"(%v); it was left alone rather than treated as asleep - retry, and if it persists `sbx rm` "+
			"the sandbox", ref, describeTimeout, err)
	}
}

func (p *fcProvider) Start(ctx context.Context, ref string) error {
	// Lock, THEN load: a record read before the lock can be one a concurrent sleep is about to
	// replace, and acting on its stale SnapshotValid=false cold-boots over a fresh snapshot.
	unlock := p.lock(ref)
	defer unlock()

	vm, err := p.load(ref)
	if err != nil {
		return err
	}

	state, err := p.running(ctx, ref)
	if err != nil {
		return err
	}

	switch state {
	case fc.StateRunning:
		return nil
	case fc.StatePaused:
		return p.client(ref).Resume(ctx)
	}

	dir := p.dir(ref)

	// A VMM that is up but not started, or a stale process: clear it before a new one binds.
	_ = p.launch.Kill(ctx, dir)

	release, err := p.bootSlot(ctx)
	if err != nil {
		return err
	}
	defer release()

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

// bootSlot waits for one of p.boots, or ctx. The release is safe to call more than once.
func (p *fcProvider) bootSlot(ctx context.Context) (func(), error) {
	if p.boots == nil {
		return func() {}, nil
	}

	select {
	case p.boots <- struct{}{}:
		var once sync.Once

		return func() { once.Do(func() { <-p.boots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// previousExitWait bounds how long a new VMM waits for the last one in the same directory to let go.
var previousExitWait = 5 * time.Second

// awaitPrevious holds a launch until the previous firecracker in this VM's directory has exited
// and closed its files. They share a tap, and a new VMM that opens it while the old one still
// holds it fails its snapshot/load with EBUSY (seen on the first CI run on real KVM). Bounded: a
// process that will not let go is an error that names it, not a wait for ever.
func (p *fcProvider) awaitPrevious(ctx context.Context, ref string) error {
	dir := p.dir(ref)
	deadline := time.Now().Add(previousExitWait)

	for p.launch.Alive(dir) {
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: the previous firecracker in %s has not exited after %s and may still hold "+
				"this VM's tap; not starting a second one on it", ref, dir, previousExitWait)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}

	return nil
}

// restore loads the snapshot and resumes it. The caller holds the lock.
func (p *fcProvider) restore(ctx context.Context, vm *fcVM) error {
	dir := p.dir(vm.Ref)
	a := vm.addr()

	if err := p.net.EnsureTap(ctx, a); err != nil {
		return err
	}

	if err := p.awaitPrevious(ctx, vm.Ref); err != nil {
		return err
	}

	// Invalid BEFORE the resume, durably - see the file comment.
	vm.SnapshotValid = false
	if err := p.save(vm); err != nil {
		return err
	}

	clearVsock(dir)

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
		if err := p.rekey(ctx, vm); err != nil {
			_ = p.launch.Kill(context.WithoutCancel(ctx), dir)
			_ = p.save(vm)

			return fmt.Errorf("re-keying execd after the restore: %w - the VM was stopped rather "+
				"than left serving with the snapshot's identity", err)
		}
	}

	if err := p.save(vm); err != nil {
		// execd now holds a secret this record does not: left running, it could never be
		// sealed again. Stopped, its next wake is a cold boot with the boot secret.
		_ = p.launch.Kill(context.WithoutCancel(ctx), dir)

		return fmt.Errorf("recording the restored VM: %w - it was stopped", err)
	}

	return nil
}

// clearVsock removes the vsock device's unix socket a previous VMM left behind. Firecracker binds
// it itself - at boot, and again on snapshot/load - and a killed process never unlinks it, so
// without this every wake after the first fails "Address in use". The caller holds the lock and
// has killed any VMM, so nothing is listening on it.
func clearVsock(dir string) { _ = os.Remove(filepath.Join(dir, fc.VsockName)) }

func (p *fcProvider) revalidate(vm *fcVM, cause error) error {
	vm.SnapshotValid = true
	if err := p.save(vm); err != nil {
		return errors.Join(cause, err)
	}

	return cause
}

func (p *fcProvider) Stop(ctx context.Context, ref string) error {
	unlock := p.lock(ref) // before load; see Start
	defer unlock()

	vm, err := p.load(ref)
	if err != nil {
		return err
	}

	state, err := p.running(ctx, ref)
	if err != nil {
		return err
	}

	switch state {
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

	state, err := p.running(ctx, ref)
	if err != nil {
		return err
	}

	switch state {
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

	state, err := p.running(ctx, ref)
	if err != nil {
		return err
	}

	if state == fc.StatePaused {
		return p.client(ref).Resume(ctx)
	}

	return nil
}

// Healthy and Probe run the service's health command in the guest when it has one, and otherwise
// dial the guest's first port on its tap address. That is a real check, and
// declared says so: in a container a published port is docker-proxy, which accepts before the
// server behind it exists, but here nothing sits in front of the guest's own TCP stack - an
// accepted connection is a listener. Undeclared, the daemon would wait a flat 2 s on every wake
// of a snapshot whose workload answers in milliseconds.
// runHealth runs a health command once in the guest.
func (p *fcProvider) runHealth(ctx context.Context, ref, command string) error {
	run := p.healthExec
	if run == nil {
		run = p.Exec
	}

	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	_, err := run(ctx, ref, []string{"/bin/sh", "-c", command})

	return err
}

// waitHealth polls command until it passes or ctx ends.
func (p *fcProvider) waitHealth(ctx context.Context, ref, command string) error {
	for {
		err := p.runHealth(ctx, ref, command)
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("the health command %q never passed: %w", command, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// healthTimeout bounds one run of a service's health command, as docker's --health-timeout does.
const healthTimeout = 5 * time.Second

func (p *fcProvider) Healthy(ctx context.Context, ref string) (bool, bool) { return p.Probe(ctx, ref) }

func (p *fcProvider) Probe(ctx context.Context, ref string) (bool, bool) {
	vm, err := p.load(ref)
	if err != nil || (len(vm.Ports) == 0 && vm.Health == "") {
		return false, false
	}

	// Asleep is nothing to ask, not a failing check: Create leaves every VM asleep, and a caller
	// that probes right after it (the CLI's post-create wait) must not spin on a VM nobody woke.
	// The wake path probes after Start, when the VM is running.
	if state, err := p.running(ctx, ref); err != nil || state != fc.StateRunning {
		return false, false
	}

	// A health command is the service's own word on whether it serves - a database that accepts
	// before it can answer a query is the reason the spec has one - so it is what counts after a
	// cold boot, run the way docker runs a CMD-SHELL check: through /bin/sh in the guest.
	//
	// Not after a snapshot restore. Create snapshots only once the command has passed, so a
	// restored VM is that serving workload resumed mid-flight, not a process starting up; running
	// the command again was an exec round trip on every wake (measured 177-336 ms of a 340-585 ms
	// wake through the helper VM) to re-ask a question the snapshot already answered. The port
	// dial below still confirms the guest is reachable.
	if vm.Health != "" && !vm.Restored {
		return p.runHealth(ctx, ref, vm.Health) == nil, true
	}

	if len(vm.Ports) == 0 {
		return true, true // restored, health passed before the snapshot, and nothing to dial
	}

	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	dial := p.probeDial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	}

	c, err := dial(ctx, net.JoinHostPort(vm.addr().GuestIP(), strconv.Itoa(vm.Ports[0])))
	if err != nil {
		return false, true
	}

	_ = c.Close()

	return true, true
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

		if state, err := p.running(ctx, vm.Ref); err == nil && state == "" {
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

	out := "the guest console's last lines:\n" + buf.String()

	// A PID 1 that exits is a kernel panic whose call trace fills the tail, and the reason - fc-init
	// or execd saying why - scrolls out of it. Those lines come first, wherever they are.
	if why := consoleReasons(filepath.Join(dir, fc.ConsoleName)); why != "" {
		out = "what the guest said:\n" + why + out
	}

	return out
}

func consoleReasons(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	var out strings.Builder

	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "sbx fc-init:") || strings.Contains(line, "sbx execd:") ||
			strings.Contains(line, "Kernel panic") {
			out.WriteString(line + "\n")
		}
	}

	return out.String()
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

		// A VMM that is alive and slow is up, not asleep: reported running, so nothing wakes it.
		state, serr := p.running(ctx, vm.Ref)

		u := Unit{
			Sandbox: vm.Sandbox, Service: vm.Service, Slot: vm.Slot, Ref: vm.Ref,
			Instance: vm.Instance, Running: state == fc.StateRunning || serr != nil, Paused: state == fc.StatePaused,
			Index: vm.Index % blockSize, DependsOn: vm.DependsOn, Idle: vm.Idle, OnIdle: vm.OnIdle,
		}

		if vm.EgressPolicy != "" {
			a := vm.addr()
			u.EgressGateway, u.EgressBridge, u.EgressPolicy = a.Gateway(), a.Bridge(), vm.EgressPolicy
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

// releaseNet undoes what a failed Create's boot made on the host: the VM's tap, and its slot's
// bridge when no other VM is in that slot. Best effort - the create is failing already.
func (p *fcProvider) releaseNet(ctx context.Context, vm *fcVM) {
	_ = p.net.RemoveTap(ctx, vm.addr())

	vms, err := p.all()
	if err != nil {
		return
	}

	for _, o := range vms {
		if o.Slot == vm.Slot && o.Ref != vm.Ref {
			return
		}
	}

	_ = p.net.RemoveBridge(ctx, vm.Slot)
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

// snapshotDir is where Commit writes image: a readable name plus a hash of the exact one, because
// the readable part alone folds "a/b:c" and "a_b_c" into one directory and a Commit of either
// would overwrite the other's snapshot.
func (p *fcProvider) snapshotDir(image string) string {
	h := sha256.Sum256([]byte(image))

	return p.legacySnapshotDir(image) + "-" + hex.EncodeToString(h[:6])
}

// legacySnapshotDir is the name before the hash (v0.10), still read so those snapshots restore.
func (p *fcProvider) legacySnapshotDir(image string) string {
	return filepath.Join(p.root, "snapshots", strings.NewReplacer("/", "_", ":", "_").Replace(image))
}

// savedSnapshotDir is the directory holding image's snapshot: the hashed one, or a v0.10 one whose
// name file says it is exactly this image and not another that folds to the same name.
func (p *fcProvider) savedSnapshotDir(image string) (string, bool) {
	for _, d := range []string{p.snapshotDir(image), p.legacySnapshotDir(image)} {
		if name, ok := p.snapshotByDir(d); ok && name == image {
			return d, true
		}
	}

	return "", false
}

type fcSnapshot struct {
	VM  fcVM   `json:"vm"`
	Dir string `json:"dir"` // the VM directory it was taken in: drive paths in vm.state point there
}

func (p *fcProvider) snapshotFor(image string) (*fcSnapshot, bool) {
	dir, ok := p.savedSnapshotDir(image)
	if !ok {
		return nil, false
	}

	b, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
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

	unlock := p.lock(ref) // before load; see Start
	defer unlock()

	vm, err := p.load(ref)
	if err != nil {
		return err
	}

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

	state, err := p.running(ctx, ref)
	if err != nil {
		return err
	}

	// The record the snapshot is saved with: the identity execd holds inside it.
	snap := *vm

	switch state {
	case fc.StateRunning, fc.StatePaused:
		if snap, err = p.commitLive(ctx, vm, state, dst, copyVM); err != nil {
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

	b, err := json.Marshal(fcSnapshot{VM: snap, Dir: dir})
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dst, "snapshot.json"), b, 0o600)
}

// commitLive snapshots a VM that is up, into dst, and leaves it as it found it.
//
// execd is sealed first, exactly as for a sleep: the snapshot is a second copy of its identity,
// and one taken unsealed would restore already serving with it. It is re-keyed after, with a fresh
// control secret, so the running VM and the saved one never hold the same secret. A frozen VM
// (on_idle: freeze) is thawed only for execd to answer the seal and is frozen again at the end;
// without a guest agent it is never thawed at all. It returns the record the snapshot holds.
func (p *fcProvider) commitLive(ctx context.Context, vm *fcVM, state, dst string, copyVM func(bool) error) (fcVM, error) {
	c := p.client(vm.Ref)
	guest := p.guest.Available()

	if guest {
		if state == fc.StatePaused {
			if err := c.Resume(ctx); err != nil {
				return fcVM{}, err
			}
		}

		if err := p.seal(ctx, vm); err != nil {
			// Sealed or not, it cannot be trusted to serve: stopped, its next wake cold-boots.
			return fcVM{}, errors.Join(fmt.Errorf("sealing execd before the snapshot: %w - the VM was "+
				"stopped, and its next wake is a cold boot", err), p.abandon(ctx, vm))
		}
	}

	if guest || state == fc.StateRunning {
		if err := c.Pause(ctx); err != nil {
			return fcVM{}, errors.Join(err, p.abandon(ctx, vm))
		}
	}

	err := c.CreateSnapshot(ctx, fc.SnapshotCreate{SnapshotType: fc.SnapshotFull,
		SnapshotPath: filepath.Join(dst, fc.StateName), MemFilePath: filepath.Join(dst, fc.MemName)})
	if err == nil {
		err = copyVM(false)
	}

	snap := *vm

	// A snapshot resets Firecracker's dirty bitmap; the next sleep must be Full.
	vm.Restored = false

	// Running again only if it was running - or was thawed for the seal and must be re-keyed.
	if guest || state == fc.StateRunning {
		if rerr := c.Resume(ctx); rerr != nil {
			return fcVM{}, errors.Join(err, rerr, p.abandon(ctx, vm))
		}
	}

	if guest {
		vm.Generation++

		if rerr := p.rekey(ctx, vm); rerr != nil {
			return fcVM{}, errors.Join(err, fmt.Errorf("re-keying execd after the snapshot: %w - the VM "+
				"was stopped rather than left sealed", rerr), p.abandon(ctx, vm))
		}

		if state == fc.StatePaused {
			if perr := c.Pause(ctx); perr != nil && err == nil {
				err = perr
			}
		}
	}

	if serr := p.save(vm); serr != nil {
		// execd holds a secret this record does not: stopped, its next wake is a cold boot.
		return fcVM{}, errors.Join(err, serr, p.abandon(ctx, vm))
	}

	return snap, err
}

// createFromSnapshot restores a saved VM as a new service - only where its paths and address
// still hold. A Firecracker snapshot bakes in the host paths of its drives and the guest's IP
// (configured by the kernel at the original boot), so it can come back as the same sandbox and
// service in the same slot, and nowhere else, until drives are re-pointed per clone (a jailer or
// a mount namespace) and the guest agent re-addresses the network. A clone under another name
// is also a fork, which needs the guest to re-key - refused for both reasons, each stated.
func (p *fcProvider) createFromSnapshot(_ context.Context, s *fcSnapshot, svc spec.Service, ref string, slot int, eps []Endpoint) error {
	dir, image := p.dir(ref), svc.Image

	switch {
	case s.Dir != dir:
		return fmt.Errorf("the firecracker snapshot for %s/%s can only be restored as that same "+
			"sandbox and service: its drive paths are baked into the VM state, and restoring it "+
			"under another name would be a fork, which sbx refuses", s.VM.Sandbox, s.VM.Service)
	case s.VM.Slot != slot:
		return fmt.Errorf("the firecracker snapshot for %s was taken in slot %d, and this sandbox "+
			"got slot %d: the guest's address is part of its memory. Free slot %d and retry",
			ref, s.VM.Slot, slot, s.VM.Slot)
	case svc.Filtered() != (s.VM.EgressPolicy != ""):
		// HTTP(S)_PROXY is in the environment of every process the snapshot holds, set or not
		// at its first boot. Restoring a filtered snapshot as an unfiltered service would leave
		// clients pointed at a filter nobody serves; the other way round, at none at all, with
		// the filter serving nothing. Neither is the spec, so neither is done.
		return fmt.Errorf("the firecracker snapshot for %s was taken %s, and the spec now asks for "+
			"it %s: the proxy setting is in the environment of every process in the snapshot's "+
			"memory. Keep the snapshot's egress, or remove the snapshot and create afresh",
			ref, filteredWord(s.VM.EgressPolicy != ""), filteredWord(svc.Filtered()))
	}

	// Create holds the lock and removes dir if this fails.
	src, ok := p.savedSnapshotDir(image)
	if !ok {
		return fmt.Errorf("the firecracker snapshot %s is gone", image)
	}

	for _, f := range []string{fc.RootfsName, "agent.ext4", fc.StateName, fc.MemName} {
		if _, err := fc.CloneFile(filepath.Join(src, f), filepath.Join(dir, f)); err != nil {
			return fmt.Errorf("restoring %s from its snapshot: %w", f, err)
		}
	}

	vm := s.VM
	vm.Instance = randomHex(8)
	vm.Created = time.Now().UTC()
	vm.SnapshotValid = true
	vm.Restored = false
	vm.Public = nil

	// The policy itself may differ from the snapshot's: it is held by the filter, not the guest.
	vm.EgressPolicy = ""

	if svc.Filtered() {
		var err error
		if vm.EgressPolicy, err = declaredJSON(svc); err != nil {
			return err
		}
	}

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
	dir, ok := p.savedSnapshotDir(image)
	if !ok {
		return nil
	}

	return os.RemoveAll(dir)
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
