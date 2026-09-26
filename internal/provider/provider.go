package provider

// A provider is somewhere sandboxes can exist: this laptop's Docker, a cluster, later a
// pool of microVMs. The seam is here because two things about a sandbox are not portable,
// and everything else is.
//
// **Addressing is not portable.** Locally every sandbox shares one loopback, so services
// get remapped into a per-slot port block. In a cluster every pod has its own address, so
// MySQL is simply :3306 on a name - port arithmetic is a workaround for a constraint that
// does not exist there. Callers therefore ask for an *endpoint*, never a port, and the
// provider decides what that means.
//
// **Isolation is not portable either.** On one laptop a container is the right trade: an
// escape reaches your own machine and a microVM's memory competes with your editor. On
// shared infrastructure an escape reaches other people's sandboxes and the memory is not
// scarce, so the correct answer inverts. That is a declared choice, not a rewrite.
//
// What *is* portable is the policy: what exists, how you know it is serving, and when it
// should sleep. Those are the same sentences in both worlds, which is why the daemon's
// wake logic talks to this interface rather than to Docker.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

const (
	labelSandbox = "sbx.sandbox" // which sandbox a container belongs to
	labelSlot    = "sbx.slot"    // its port block
	labelService = "sbx.service" // its name within the sandbox
	labelPorts   = "sbx.ports"   // public:backing pairs sbx serve should front

	labelEgressAllow   = "sbx.egress.allow"   // comma-joined egress allow-list, when set
	labelEgressGateway = "sbx.egress.gateway" // the bridge gateway its egress filter listens on
	labelEgressStat    = "sbx.egress.stat"    // loopback address of a container filter's activity endpoint
	labelEgressPolicy  = "sbx.egress.policy"  // the egress policy the spec declared, as JSON
	labelEgressToken   = "sbx.egress.token"   // the secret a container filter's control endpoint requires
	labelIdle          = "sbx.idle"           // per-service idle override, when set
	labelDependsOn     = "sbx.dependsOn"      // comma-joined depends_on, so wake can follow it
	labelOnIdle        = "sbx.onIdle"         // "freeze" when idle should pause rather than stop
	labelOSB           = "sbx.osb"            // set on containers the OpenSandbox API created: whose they are

	// Kubernetes label keys are stricter than docker's, so the cluster side uses its own
	// names rather than risking a silently rejected manifest.
	kubeLabelSandbox   = "sbx-sandbox"
	kubeLabelService   = "sbx-service"
	kubeLabelSlot      = "sbx-slot"
	kubeLabelOrdinal   = "sbx-ordinal"
	kubeLabelManagedBy = "sbx-managed-by"
)

// Endpoint is how a caller reaches one port of one service.
type Endpoint struct {
	Host string
	Port int
}

func (e Endpoint) String() string { return fmt.Sprintf("%s:%d", e.Host, e.Port) }

// Unit is one wakeable service as the provider currently sees it.
type Unit struct {
	Sandbox string
	Service string
	Slot    int
	Ref     string // provider-local handle: container id, or namespace/deployment
	Running bool

	// Instance changes when the thing behind Ref is replaced, and Ref does not.
	//
	// A docker Ref is the container's *name*, which sbx derives from the sandbox and service,
	// so `sbx rm x && sbx create x` produces the identical Ref on the identical port with a
	// brand-new empty volume. Anything holding a reference across that - a tunnel, a cached
	// map - would carry on addressing what it believes is the old service. The container ID
	// does change, which is what makes it an identity rather than a label.
	Instance string

	// Index is the service's ordinal within the sandbox. Only a provider that shares one
	// address space needs it; the rest report 0 and nothing asks again.
	Index int

	// Client is where callers connect: a loopback port on a laptop, a Service name in a
	// cluster. Listen is what the daemon binds. Upstream is what it dials once the workload
	// is serving.
	//
	// Three rather than two, because in a cluster the address a client uses and the port the
	// activator binds are genuinely different things: the client keeps :3306 on a stable
	// name while the activator multiplexes many sandboxes onto one pod.
	Client   []Endpoint
	Listen   []int
	Upstream []Endpoint

	// EgressAllow and EgressGateway carry a service's egress allow-list to the daemon, which
	// runs a filtering proxy for it on the gateway. Both empty when there is no allow-list.
	EgressAllow   []string
	EgressGateway string

	// EgressStat is the loopback address of a container filter's activity endpoint, or "".
	//
	// Set only where the filter runs as a container - the daemon cannot see into the sandbox's
	// bridge there, so it scrapes this instead of watching its own listener.
	EgressStat string

	// EgressPolicy is the egress policy the spec declared for this service, as JSON, or "" for
	// a service created before policies existed (whose EgressAllow is then the whole story).
	// It is what the filter starts with and what a reset returns to; a policy changed on the
	// running service lives with the filter, not here.
	EgressPolicy string

	// EgressBridge is the host bridge the gateway is on when that bridge is sbx's own - a
	// microVM sandbox's sbxfc<slot> - rather than one docker made; "" otherwise. Such a bridge
	// comes and goes with its VMs (a host reboot takes it until the next wake), so the daemon
	// binds the filter there even while the address is absent. And because that filter runs on
	// the very host the guests are kept off, it refuses the host's own addresses and the rest of
	// the VM plan unless a rule names them.
	EgressBridge string

	// DependsOn is what this service declared it needs. Carried to the daemon so that waking
	// it wakes those too: a stopped container is absent from the network's DNS, so a service
	// woken without its dependencies dials a name that does not resolve.
	DependsOn []string

	// Idle is a per-service idle override ("never", "0", or a duration), empty for the global
	// default. It lets the daemon keep a box awake while an agent works inside it with no traffic.
	Idle string

	// Paused is true when the workload is frozen in place - processes and memory kept, no CPU -
	// rather than stopped. Running is false for a paused unit, because nothing it holds can
	// answer; Paused is what tells the wake path to thaw it instead of starting it, since a
	// runtime asked to start a frozen workload refuses.
	Paused bool

	// OnIdle is what the daemon does to this unit when it goes quiet: "" stops it (the default,
	// and what every sandbox from a sandbox.json gets), "freeze" pauses it. See spec.Service.OnIdle.
	OnIdle string

	// OSB is the sbx.osb label: non-empty on a container created through the OpenSandbox API. A
	// daemon that does not serve the API, and was not scoped to include them, leaves these alone.
	OSB string
}

// EgressProxyPort is where a sandbox's egress filter listens on its no-NAT bridge gateway. The
// provider injects it into HTTP_PROXY and the daemon binds it; one constant so they agree.
const EgressProxyPort = 20999

// Isolation is how strongly a sandbox is separated from its host and its neighbours.
type Isolation string

const (
	// IsolationContainer shares the host kernel. Correct on a single-user machine.
	IsolationContainer Isolation = "container"

	// IsolationGVisor runs a user-space kernel (runsc). Correct on shared infrastructure,
	// where an escape would reach somebody else's work.
	IsolationGVisor Isolation = "gvisor"

	// IsolationKata gives each sandbox a real VM. The strongest of the three and the only
	// one that makes "run anything, no restriction" safe to say out loud in public.
	IsolationKata Isolation = "kata"

	// IsolationFirecracker gives each sandbox a Firecracker microVM. On kubernetes it is kata's
	// Firecracker RuntimeClass; locally the microVM is --provider firecracker instead, which is
	// a provider rather than a runtime because it owns the whole lifecycle, snapshots included.
	IsolationFirecracker Isolation = "firecracker"
)

func (i Isolation) Valid() bool {
	switch i {
	case IsolationContainer, IsolationGVisor, IsolationKata, IsolationFirecracker:
		return true
	default:
		return false
	}
}

// Provider is the whole surface a sandbox backend has to implement.
type Provider interface {
	// Name is what appears in errors and in `sbx list`.
	Name() string

	// Create realises one service. It may run the workload briefly to initialise it; it
	// must not otherwise manage run state, which belongs to the wake policy alone.
	Create(ctx context.Context, sandbox string, slot, ordinal int, service string, svc spec.Service, eps []Endpoint, specDir string, iso Isolation) error

	// Start and Stop are the wake verbs: `docker start` here, `scale 1` there.
	Start(ctx context.Context, ref string) error
	Stop(ctx context.Context, ref string) error

	// Healthy reports the platform's own opinion: cheap, and behind. Docker only republishes
	// a container's health on its check interval, and on this machine that lag measured
	// 5030ms against a Redis that was serving in 110ms.
	//
	// declared=false means the caller is about to guess, and every caller treats that as
	// worth saying out loud.
	Healthy(ctx context.Context, ref string) (serving, declared bool)

	// Probe runs the readiness check now and returns what it actually said.
	//
	// This exists because Healthy is the wrong question on the wake path. The caller is
	// holding an open connection while it waits, so it wants the truth at its own cadence,
	// not at the platform's - the difference between the two was 98% of a wake.
	Probe(ctx context.Context, ref string) (serving, declared bool)

	// List returns every unit of a sandbox, or of all sandboxes when sandbox is empty.
	List(ctx context.Context, sandbox string) ([]Unit, error)

	// Remove destroys a sandbox including its persistent storage.
	Remove(ctx context.Context, sandbox string) error

	// Exec runs argv inside a service and returns what it printed.
	//
	// argv, not a string. Joining arguments and handing them to `sh -c` loses the quoting
	// the caller already got right: `psql -c "CREATE TABLE t (id int)"` arrives at the shell
	// unquoted and dies on the parenthesis. Anything that genuinely wants a shell asks for
	// one by passing {"sh", "-c", ...}.
	Exec(ctx context.Context, ref string, argv []string) (string, error)

	// ExecTTY is Exec with a terminal attached, wired straight to this process's stdio.
	// It is a separate method rather than a flag because the two have different shapes:
	// Exec captures output and returns it, this one hands the terminal over and returns
	// only when the user is done. Trying to be both is how a shell ends up with no
	// echo and no job control.
	ExecTTY(ctx context.Context, ref string, argv []string) error

	// Logs writes a service's output to w, optionally following it.
	//
	// A writer rather than a string because following has no end: a sandbox is a set of
	// processes and you want to watch them the way you watch a server.
	Logs(ctx context.Context, ref string, lines int, follow bool, w io.Writer) error

	// Copy moves a file in or out. Exactly one of src/dst is inside the sandbox, written as
	// ":path"; the other is a host path.
	Copy(ctx context.Context, ref, src, dst string) error

	// Endpoints decides addressing for a service that is about to be created.
	Endpoints(sandbox, service string, slot, startIndex int, containerPorts []int) []Endpoint

	// AllocSlot reserves a port block where the provider needs one. A provider that gives
	// every sandbox its own address space can return 0 forever.
	AllocSlot(ctx context.Context, sandbox string) (int, error)
}

// providerFor resolves the --provider flag.
func For(kind, socket, namespace string) (Provider, error) {
	switch kind {
	case "", "docker":
		ep, err := resolveDockerHost(socket)
		if err != nil {
			return nil, err
		}

		return newDocker(ep), nil
	case "kubernetes", "k8s":
		return newKube(namespace), nil
	case "firecracker", "fc":
		return forFirecracker(socket)
	default:
		return nil, fmt.Errorf("unknown provider %q (want docker, kubernetes or firecracker)", kind)
	}
}

// Capabilities beyond the core.
//
// Not every backend can do everything, and the ones that cannot should not be made to write
// stubs that return errors - a method on the core interface is a promise that every provider
// keeps, and four methods that only docker implements is not an interface, it is a docker
// client with a kubernetes-shaped hole in it.
//
// These are optional interfaces instead. A provider implements one if it can do the thing
// natively; the CLI asks with a type assertion and reports a single clear refusal if not.
// It is the same negotiation --isolation already uses: declare what you want, and be told
// plainly when this backend cannot give it to you.
//
// The rule for adding one: the capability is named for what the USER wants, never for how a
// backend happens to do it. Snapshotter, not Committer - because the kubernetes answer is a
// volume snapshot through its own CSI, not `docker commit`, and an interface named after
// docker's verb would have made that implementation look like a workaround.

// Snapshotter saves and restores a service's state. Filesystem state - memory and running
// processes are not included, and the docs say so wherever the word snapshot appears.
type Snapshotter interface {
	// Commit saves a unit's filesystem as a named image. changes are Dockerfile-style
	// instructions applied to the image config (docker commit --change), e.g. "ENV K=" to
	// clear a variable that belongs to the running unit rather than to its filesystem.
	Commit(ctx context.Context, ref, image string, changes ...string) error

	// Images lists saved images beginning with prefix.
	Images(ctx context.Context, prefix string) ([]string, error)

	// CopyVolume replicates one volume into another, creating the destination.
	CopyVolume(ctx context.Context, src, dst string) error

	// VolumeFor names the volume a service's data lives in.
	VolumeFor(sandbox, service string) string

	// RemoveImage deletes a saved image; one already gone is success. An image still used by
	// a unit is refused by the backend, and that refusal is returned rather than forced.
	RemoveImage(ctx context.Context, image string) error
}

// SnapshotterFor returns the provider's snapshot support, or a refusal naming the backend.
func SnapshotterFor(p Provider) (Snapshotter, error) {
	s, ok := p.(Snapshotter)
	if !ok {
		return nil, fmt.Errorf("the %s provider cannot snapshot: saving and restoring a "+
			"service's state is not something it can do natively, and sbx will not reach "+
			"around it to do so", p.Name())
	}

	return s, nil
}

// NamedVolumes is storage a caller names and keeps: a docker named volume, a claim in a cluster.
//
// Separate from the data volume every service with `volume` already has, because that one
// belongs to its sandbox and dies with it, and these do not: a named volume outlives any one
// sandbox that mounts it unless whoever created it asks otherwise. Kubernetes does not
// implement it yet - a PersistentVolumeClaim is the answer there, with a storage class and a
// size that are the operator's decisions - so the API says so rather than guessing them.
type NamedVolumes interface {
	// VolumeExists reports whether name exists. An error is not an absence.
	VolumeExists(ctx context.Context, name string) (bool, error)

	// CreateVolume creates name with labels.
	CreateVolume(ctx context.Context, name string, labels map[string]string) error

	// RemoveVolume deletes name; a volume still mounted is refused by the backend.
	RemoveVolume(ctx context.Context, name string) error
}

// NamedVolumesFor returns the provider's named-volume support, or a refusal naming the backend.
func NamedVolumesFor(p Provider) (NamedVolumes, error) {
	v, ok := p.(NamedVolumes)
	if !ok {
		return nil, fmt.Errorf("the %s provider cannot create named volumes: on a cluster that "+
			"is a PersistentVolumeClaim, whose storage class and size are the operator's to "+
			"choose, and sbx does not create one for you yet", p.Name())
	}

	return v, nil
}

// Checkpointer saves and restores a running unit's MEMORY and process state, so a resume
// picks up where the workload left off instead of cold-starting against a warm disk. This is
// the one thing E2B and zeropod do that Snapshotter does not: an agent's half-finished REPL,
// a warmed cache, a connection mid-handshake all come back.
//
// Named for what the user wants (a checkpoint of the live process), not for how a backend
// does it. The docker answer is CRIU, reached through `docker checkpoint`, which the daemon
// exposes ONLY in experimental mode - and Docker Desktop does not, so on macOS this is
// refused with a reason rather than approximated. The kubernetes answer is a CRIU shim on the
// node (this is what zeropod is), which is the cluster operator's to install, not sbx's to
// reach around for - so the kubernetes provider does not implement this and CheckpointerFor
// says so plainly. `sbx doctor` reports whether this host can do it before you rely on it.
type Checkpointer interface {
	// Checkpoint dumps a running unit's memory and processes under a name. With leaveRunning
	// false the unit is frozen (stopped) as the dump is taken, which is the CRIU default and
	// what "park a REPL to resume later" wants; true dumps a copy and keeps it serving.
	Checkpoint(ctx context.Context, ref, name string, leaveRunning bool) error

	// Restore starts a unit from a named checkpoint, resuming its memory and processes.
	Restore(ctx context.Context, ref, name string) error

	// Checkpoints lists the checkpoint names saved for a unit.
	Checkpoints(ctx context.Context, ref string) ([]string, error)
}

// CheckpointerFor returns the provider's memory-checkpoint support, or a refusal naming the
// backend. A provider implementing the interface may still refuse at call time when the host
// lacks CRIU - the type check only says the backend has a path to it at all.
func CheckpointerFor(p Provider) (Checkpointer, error) {
	c, ok := p.(Checkpointer)
	if !ok {
		return nil, fmt.Errorf("the %s provider cannot checkpoint memory: saving a running "+
			"process's state needs a CRIU shim under the runtime, which on a cluster is the "+
			"operator's to install (that is what zeropod is) and not something sbx will reach "+
			"around it to do", p.Name())
	}

	return c, nil
}

// Pauser freezes a running unit in place and thaws it again: processes, open files and memory
// are kept, and it uses no CPU while frozen.
//
// Named for what the user wants - "stop it costing CPU without losing what it was doing" - not
// for how docker does it (the cgroup freezer, behind `docker pause`). It is not Checkpointer: a
// checkpoint survives the container, a pause does not survive a reboot, and a pause needs no
// CRIU and costs about 10 ms either way.
//
// Kubernetes does not implement it. A pod cannot be frozen through the API - scaling to zero
// throws the memory away - and calling that "pause" would be the stub this file exists to avoid.
type Pauser interface {
	// Pause freezes a running unit. Pausing one that is already paused is success.
	Pause(ctx context.Context, ref string) error

	// Unpause thaws a paused unit. Thawing one that is not paused is success: the caller wants
	// it running, and a unit that was never frozen is as thawed as it gets. A unit that is
	// STOPPED is not thawed by this - the wake path finds that out on its next dial and starts it.
	Unpause(ctx context.Context, ref string) error
}

// PauserFor returns the provider's pause support, or a refusal naming the backend.
func PauserFor(p Provider) (Pauser, error) {
	pa, ok := p.(Pauser)
	if !ok {
		return nil, fmt.Errorf("the %s provider cannot pause a sandbox: freezing a workload with "+
			"its memory kept is not something it can do natively (scaling to zero discards the "+
			"memory), and sbx will not call that a pause", p.Name())
	}

	return pa, nil
}

// ImageInfo is what an image says it runs, and on what.
type ImageInfo struct {
	Entrypoint []string
	Cmd        []string
	OS         string
	Arch       string // GOARCH spelling: amd64, arm64
}

// ImageInspector reads what an image runs, and on what. Injector carries it for docker; a
// provider that runs the agent itself (RunsAgent) answers it too, because the OpenSandbox API
// records a sandbox's command from it whichever way the agent gets in.
type ImageInspector interface {
	ImageInfo(ctx context.Context, image string) (ImageInfo, error)
}

// RunsAgent is implemented by a provider whose every sandbox already runs sbx's agent, execd, as
// the sandbox's agent: a microVM, whose PID 1 (`sbx fc-init`) becomes execd with the workload as
// its child. Named for the want - "this sandbox answers execd's API without being given it" -
// not for how: a provider that bakes the agent in some other way is the same capability.
//
// The OpenSandbox API skips, for such a provider, everything Injector is for: seeding execd into
// a volume, mounting it at /opt/sbx, and wrapping the entrypoint in it. What it keeps is the
// token. The API mints the sandbox's execd access token and puts it in the spec's env as
// EXECD_ACCESS_TOKEN; a RunsAgent provider boots execd with that token, and re-keys a restored
// execd with it, and never mints one of its own for such a sandbox - two tokens for one sandbox
// is a sandbox the API hands out credentials for that its agent refuses.
type RunsAgent interface {
	ImageInspector

	// RunsAgent is a marker: implementing it is the promise above.
	RunsAgent()
}

// AgentTokenEnv is the env var the API puts the execd access token in, and the one execd reads.
const AgentTokenEnv = "EXECD_ACCESS_TOKEN"

// HostVolumes is implemented by a provider that can bind a directory of this machine into a
// sandbox. Docker can; a microVM cannot (Firecracker has no virtio-fs), and the OpenSandbox API
// refuses a `host` volume by name on a provider without it rather than creating a sandbox whose
// mount silently is not there.
type HostVolumes interface {
	HostVolumes()
}

// HostVolumesFor returns nil when p can mount host directories, or the refusal naming it.
func HostVolumesFor(p Provider) error {
	if _, ok := p.(HostVolumes); ok {
		return nil
	}

	if p.Name() == "firecracker" {
		return errors.New("the firecracker provider cannot mount a host directory: a microVM's " +
			"only way to share one would be virtio-fs, which Firecracker does not have - use a pvc " +
			"volume (an ext4 image attached as a drive), or the docker provider")
	}

	return fmt.Errorf("the %s provider cannot mount a host directory into a sandbox", p.Name())
}

// Injector runs a program sbx supplies inside an image that does not carry it.
//
// This is how a sandbox created through the OpenSandbox API gets its agent (`sbx execd`) into
// `python:3.11-slim`, or any other image the caller names: the binary goes into a named volume
// once, the volume is mounted read-only into every sandbox that needs it, and the image itself is
// never modified or rebuilt.
//
// Optional like the rest. A cluster would do this with an init container copying from an image
// it can pull, which is a different mechanism with a different trust story - the kubernetes
// provider does not implement it, and API sandboxes are refused there with that reason.
type Injector interface {
	// ImageInfo reads an image's default command and platform. The image must be present.
	ImageInspector

	// VolumeRuns reports whether volume already holds an executable at name that runs inside
	// image - the check is to run it, because a file that is present but built for the wrong
	// architecture is the failure this exists to catch.
	VolumeRuns(ctx context.Context, volume, name, image string) bool

	// SeedFile creates volume if needed and copies hostPath into it as name, executable. image
	// is any image present locally; it is only used as the container the copy goes through,
	// and is never started.
	SeedFile(ctx context.Context, volume, name, hostPath, image string) error

	// SeedFromImage creates volume if needed and fills it with the contents of dir in image.
	SeedFromImage(ctx context.Context, volume, image, dir string) error
}

// ErrOSBOnFirecracker is why `sbx serve --provider firecracker --osb-addr` is refused at startup
// where the microVMs run in a helper VM (a Mac, Windows): the API sandboxes would be created one
// level down, and the host half does not front the API into the VM yet. On a Linux host with
// /dev/kvm the API serves microVMs directly (RunsAgent).
var ErrOSBOnFirecracker = errors.New("--osb-addr with --provider firecracker through a helper VM: the " +
	"OpenSandbox API serves microVMs on a Linux host with /dev/kvm, and this machine runs them inside " +
	"a helper VM, which the API is not fronted into yet. Serve the OpenSandbox API from a docker-backed " +
	"`sbx serve --osb-addr` here, or run `sbx serve --provider firecracker --osb-addr` on Linux")

// InjectorFor returns the provider's injection support, or a refusal naming the backend and
// what it would take.
func InjectorFor(p Provider) (Injector, error) {
	in, ok := p.(Injector)
	if ok {
		return in, nil
	}

	switch p.Name() {
	case "firecracker":
		return nil, errors.New("the firecracker provider does not inject sbx's agent into an image: " +
			"its VMs run the agent as PID 1 already (RunsAgent)")
	default:
		return nil, fmt.Errorf("the %s provider cannot run sbx's agent inside an arbitrary image: "+
			"on a cluster that is an init container copying from a pullable image, which sbx "+
			"does not create for you yet - use the docker provider for OpenSandbox API sandboxes", p.Name())
	}
}

// Artifact is something a sandbox left behind.
type Artifact struct {
	Kind     string // "volume" or "image"
	Name     string
	Sandbox  string        // the sandbox it belonged to, where that is knowable
	Age      time.Duration // since it was created
	Snapshot bool          // made deliberately, by name, and outliving its sandbox is the point
}

// Collector finds and removes what sandboxes leave behind.
//
// Optional, like Snapshotter: a backend implements it if reclaiming is something it can do
// natively. The kubernetes answer is a PVC and a storage class's reclaim policy, which is
// the cluster operator's decision and not sbx's to make on their behalf.
type Collector interface {
	// Orphans lists artifacts whose sandbox no longer exists. It never returns anything
	// belonging to a live sandbox, asleep or awake - asleep is the normal state here.
	Orphans(ctx context.Context) ([]Artifact, error)

	// Reclaim removes one artifact.
	Reclaim(ctx context.Context, a Artifact) error
}

// CollectorFor returns the provider's reclamation support, or a refusal naming the backend.
func CollectorFor(p Provider) (Collector, error) {
	c, ok := p.(Collector)
	if !ok {
		return nil, fmt.Errorf("the %s provider cannot reclaim artifacts: on a cluster that is "+
			"a PVC's reclaim policy and the storage class behind it, which is the operator's "+
			"decision rather than something sbx should make for them", p.Name())
	}

	return c, nil
}

// Builder makes an image from a directory instead of pulling one.
//
// Optional, and kubernetes does not implement it on purpose: building in a cluster means
// pushing to a registry the cluster can pull from, which needs credentials and a decision
// about where images live. That is the operator's, and DECISIONS.md already records the
// same reasoning for snapshots.
type Builder interface {
	// Build produces tag from contextDir, using dockerfile relative to it.
	Build(ctx context.Context, tag, contextDir, dockerfile string) error

	// HasImage reports whether tag is already present, so an unchanged context is free.
	HasImage(ctx context.Context, tag string) (bool, error)
}

// BuilderFor returns the provider's build support, or a refusal naming the backend.
func BuilderFor(p Provider) (Builder, error) {
	b, ok := p.(Builder)
	if !ok {
		return nil, fmt.Errorf("the %s provider cannot build images: in a cluster that means "+
			"pushing to a registry it can pull from, which needs credentials and a decision "+
			"about where images live - build it yourself and name it with `image`", p.Name())
	}

	return b, nil
}

// Usage is one raw sample of what a service is costing right now.
//
// Raw counters rather than percentages on purpose. CPU use is a rate, and a rate needs two
// samples and the interval between them; a provider that returned "17%" would have had to
// pick that interval itself, cache the previous sample somewhere, and be wrong for every
// caller that wanted a different one. Handing back the counters lets the dashboard compute
// the rate over exactly the interval it redraws at, and lets a test compute it over an
// interval it made up.
type Usage struct {
	// CPUNanos is cumulative CPU time consumed by the service since it started.
	CPUNanos uint64

	// SystemNanos is cumulative CPU time across the whole host over the same period. The
	// ratio of the two deltas is the share of one machine; multiplied by OnlineCPUs it is the
	// share of one core, which is the number people recognise from `docker stats`.
	SystemNanos uint64
	OnlineCPUs  int

	// MemBytes is resident memory with the page cache taken off. Docker's own `stats` does
	// the same subtraction: leaving it in reports a database that has read a large table as
	// though it were holding all of it, which is the number people would act on.
	MemBytes uint64
	MemLimit uint64
}

// Limits is what a service is allowed, as against what it is using.
//
// Kept apart from Usage because the two have different lifetimes and different costs. Usage
// is sampled every second and is expected to change; a limit is fixed when the container is
// created and is only worth fetching when the reader asks to look at one service.
//
// A limit is also the thing that makes a usage figure mean anything. "86.8%" is a share of
// one core, so on an eight-core machine it is about a ninth of the host - unless the service
// is capped at one core, in which case it is nearly full. The same number, two opposite
// readings, and nothing on screen to tell them apart.
type Limits struct {
	// NanoCPUs is the ceiling in billionths of a core, the unit docker stores it in:
	// 500000000 is half a core. Zero means uncapped.
	NanoCPUs int64

	// MemBytes is the memory ceiling, and zero means uncapped.
	//
	// Zero rather than the host's memory, which is what docker's stats endpoint reports for
	// an uncapped container. Passing that on as a denominator would say a redis holding 3 MB
	// on a laptop is "0.04% full", which is arithmetic nobody asked for about a limit that
	// does not exist.
	MemBytes uint64
}

// Capped reports whether anything is actually capped, so a caller can tell "allowed nothing"
// from "allowed everything" without repeating the zero-means-unlimited rule at every use.
func (l Limits) Capped() bool { return l.NanoCPUs > 0 || l.MemBytes > 0 }

// Host is what the machine the sandboxes run on has, so that what they are using can be read as
// a proportion of something rather than as a number on its own.
//
// The machine, not the laptop. On macOS and Windows a Linux container runs inside a VM, and it
// is the VM's cores and memory that the sandboxes actually contend for - a Mac with 32 GB whose
// colima was given 8 is a machine where 6 GB of sandboxes is nearly full, and "6 of 32" there
// would be a comforting number that is not about anything.
type Host struct {
	Cores    int
	MemBytes uint64
	Name     string // what the runtime calls it - "colima", "docker-desktop", a hostname
}

// Hoster is an optional capability: a backend that can say what the machine has.
//
// Optional because a cluster cannot answer it in the same sense. "The host" of a sandbox
// scheduled across a fleet is a question with no single answer, and inventing one - the node
// this pod happens to be on - would be a figure that changes when nothing did.
type Hoster interface {
	Host(ctx context.Context) (Host, error)
}

// Neighbour is one container sharing this machine, ours or somebody else's.
type Neighbour struct {
	Name     string
	Ours     bool // created by sbx
	Running  bool
	MemBytes uint64
}

// Neighbours is an optional capability: everything on the machine, not only what sbx made.
//
// It exists because "what is using the memory" is rarely answered by the sandboxes alone. A
// laptop's container runtime holds whatever else the day has left there, and a dashboard that
// lists only its own services will report a nearly empty machine while the VM is full.
//
// Memory only. It is instantaneous - one reading is an answer - where cpu is a rate that needs
// two samples an interval apart, and paying for that across every container on the machine to
// fill a pane somebody has open is a cost with a poor return.
type Neighbours interface {
	Neighbours(ctx context.Context) ([]Neighbour, error)
}

// Limiter reports what one service is allowed to use.
//
// Optional like the rest, and per-ref rather than in a batch: the only caller shows it for
// the service the reader has selected, and asking for the whole fleet every second would be a
// round trip per service to re-learn a number that cannot change while the container lives.
type Limiter interface {
	Limits(ctx context.Context, ref string) (Limits, error)

	// SetLimits changes what a service is allowed, in place.
	//
	// In place rather than by recreating the container, because recreating one is how a
	// sandbox loses whatever was written to it since it was made - and a ceiling is a
	// property of the running thing, not of the image it came from. It applies to a sleeping
	// service too: sleep is a stopped container, and a stopped container still has a
	// HostConfig to change.
	//
	// A zero means "leave this ceiling as it is", NOT "remove it". That is docker's rule and
	// not a choice made here: its update endpoint treats an omitted or zero value as no
	// change, so a container that has a limit cannot be returned to unlimited without being
	// recreated. Verified against a live daemon - clearing is accepted, changes nothing and
	// reports success, which is the worst of the three possible behaviours. Callers that want
	// to offer "remove the limit" have to recreate, and callers that cannot must say so
	// rather than pass a zero and claim it worked.
	SetLimits(ctx context.Context, ref string, l Limits) error
}

// ParseLimits reads a cpu and a memory ceiling the way somebody would type them: cores as a
// plain number ("0.5", "2"), memory as a size ("512m", "4g", "1024k"). An empty string, or
// "none", means no ceiling.
//
// Parsed here rather than passed to docker verbatim as the spec does, because the spec is
// checked by docker at create time and told off loudly, whereas this is typed into a
// dashboard by somebody who wants to know now whether it took.
func ParseLimits(cpu, mem string) (Limits, error) {
	var l Limits

	if c := strings.TrimSpace(cpu); c != "" && c != "none" {
		cores, err := strconv.ParseFloat(c, 64)
		if err != nil || cores <= 0 {
			return l, fmt.Errorf("cpu %q is not a number of cores - try 0.5, or 2", cpu)
		}

		l.NanoCPUs = int64(cores * 1e9)
	}

	if m := strings.TrimSpace(mem); m != "" && m != "none" {
		bytes, err := parseSize(m)
		if err != nil {
			return l, err
		}

		l.MemBytes = bytes
	}

	return l, nil
}

// parseSize reads "512m", "4g", "1024k" or a plain byte count.
func parseSize(s string) (uint64, error) {
	unit := uint64(1)
	digits := strings.TrimSpace(strings.ToLower(s))

	// "512mb" and "512m" are the same thing, and somebody will type both.
	digits = strings.TrimSuffix(digits, "b")

	if digits != "" {
		switch digits[len(digits)-1] {
		case 'k':
			unit, digits = 1<<10, digits[:len(digits)-1]
		case 'm':
			unit, digits = 1<<20, digits[:len(digits)-1]
		case 'g':
			unit, digits = 1<<30, digits[:len(digits)-1]
		}
	}

	n, err := strconv.ParseFloat(strings.TrimSpace(digits), 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("memory %q is not a size - try 512m, or 2g", s)
	}

	got := uint64(n * float64(unit))

	// Docker refuses anything under 6 MB, and refuses it from inside the daemon with a
	// message about "minimum memory limit allowed" that reads like a bug in sbx.
	if got < 6<<20 {
		return 0, fmt.Errorf("memory %q is below the 6m docker will accept", s)
	}

	return got, nil
}

// Forward is one local port standing in for a remote one, once a Forwarder has bound it.
type Forward struct {
	Service string
	Local   int // the port bound on 127.0.0.1 here
	Remote  int // the port the service is on over there
}

// Forwarder binds a deployed service's ports locally and tunnels them, so a client on this
// machine reaches a sandbox that is somewhere else.
//
// Optional, and only the remote provider has it: a local sandbox's addresses are already real
// ports on this machine, so there is nothing to forward. Over `sbx ui --connect` they are not -
// the address on screen is the deployment's - and this is what makes pressing a key on a service
// enough to open psql or redis-cli against it, without a second `sbx connect` in another window.
//
// It is idempotent: forwarding a service already forwarded returns the ports it is already on
// rather than binding a second set. The forwards live until the context passed to it is done,
// which the dashboard ties to its own lifetime, so quitting closes them.
type Forwarder interface {
	Forward(ctx context.Context, ref string) ([]Forward, error)
}

// Maintainer is a provider with host-side state that can drift while nothing is being created or
// woken - firewall rules a reload removed, a log a guest keeps writing. The daemon calls Maintain
// once per discovery pass. Optional like the rest; it must be cheap when nothing has drifted.
type Maintainer interface {
	Maintain(ctx context.Context)
}

// Meter reports what running services are costing.
//
// Optional like the rest. A service that is asleep has no sample and is not an error: it is a
// stopped container, it is costing nothing, and saying so is the whole point of this project.
// Callers should treat a missing entry as zero rather than as a failure.
type Meter interface {
	// Stats samples the given refs. Refs that are not running are omitted rather than
	// reported as an error, and one unreadable ref does not fail the others - a dashboard
	// that blanks out entirely because one container died mid-refresh is worse than one that
	// shows a gap.
	Stats(ctx context.Context, refs []string) (map[string]Usage, error)
}

// MeterFor returns the provider's metering support, or a refusal naming the backend.
func MeterFor(p Provider) (Meter, error) {
	m, ok := p.(Meter)
	if !ok {
		return nil, fmt.Errorf("the %s provider cannot report cpu and memory: in a cluster that "+
			"is the metrics API and a metrics-server that may not be installed, which is the "+
			"operator's decision rather than something sbx should assume", p.Name())
	}

	return m, nil
}

// Puller fetches an image ahead of time, so the first create is not a download.
//
// Optional for the same reason as the rest: on docker this is one command against the local
// daemon, and in a cluster there is no "local" - the image has to land on whichever node the
// scheduler later picks, which means a DaemonSet whose only job is to pull. That is a
// workload sbx would be creating in the operator's cluster without being asked, so it
// refuses and says so.
// SlotPicker chooses a free slot from a caller's own view of which are taken, instead of listing
// containers itself. For a caller that already has a recent list and knows slots it has handed
// out that no list can show yet - creates still in `docker run`.
type SlotPicker interface {
	PickSlot(taken map[int]bool) (int, error)
}

// UnitGetter reads one service of one sandbox without listing every container: the cheap
// question for a caller that already knows which sandbox it means. Absent is (Unit{}, false, nil).
type UnitGetter interface {
	UnitOf(ctx context.Context, sandbox, service string) (Unit, bool, error)
}

// Warmer is a provider whose create needs more than the image to be present - a microVM's root
// filesystem, built from the image by docker export and mkfs.ext4. Warm pulls the image if it is
// absent and builds whatever else a create would, and reports whether it had anything to do.
// Prewarm uses it in place of Pull where a provider has it.
type Warmer interface {
	Warm(ctx context.Context, image string) (built bool, err error)
}

type Puller interface {
	// Pull fetches image, and is a no-op if it is already present.
	Pull(ctx context.Context, image string) error
}

// PullerFor returns the provider's prewarm support, or a refusal naming the backend.
func PullerFor(p Provider) (Puller, error) {
	pl, ok := p.(Puller)
	if !ok {
		return nil, fmt.Errorf("the %s provider cannot prewarm: there is no local image store "+
			"to warm - an image has to be on whichever node the scheduler picks, which means "+
			"a DaemonSet sbx would be creating in your cluster uninvited. Prewarm the nodes "+
			"with your own tooling, or let the kubelet pull on first create", p.Name())
	}

	return pl, nil
}

// EgressPreflighter is a provider that can answer, before anything has been created, whether
// an egress allow-list could actually be enforced on this machine.
//
// It exists because the answer is a property of the HOST, not of the spec, so `sbx validate`
// cannot reach it and the per-service check finds out too late: services are created one at a
// time and the first failure returns, so a spec whose third service carries the allow-list
// left the first two running and the sandbox half-built, with a retry that fails identically.
// Asked once up front, the refusal costs nothing and leaves nothing behind.
//
// Optional: a provider that does not implement it is one where this cannot be decided early,
// and the per-service check still applies.
type EgressPreflighter interface {
	EgressPreflight(ctx context.Context, sandbox string) error
}

// FirecrackerRuntimeClassEnv names the RuntimeClass `--isolation firecracker` asks a cluster
// for, when it was installed under something other than kata-deploy's default.
const FirecrackerRuntimeClassEnv = "SBX_KATA_FC_RUNTIMECLASS"

// FirecrackerRuntimeClass is that name: the env override, else "kata-fc".
func FirecrackerRuntimeClass(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv(FirecrackerRuntimeClassEnv)); v != "" {
		return v
	}

	return "kata-fc"
}

// ExitReporter says why a workload is not running, in the runtime's own terms: its state, its
// exit code, whether the kernel killed it for memory, and any error the runtime recorded while
// starting it. A caller that has to report a sandbox as failed asks this, because the workload's
// own output is often empty - a process killed by a signal prints nothing - and "it exited" with
// no cause is a failure nobody can act on.
type ExitReporter interface {
	ExitOf(ctx context.Context, ref string) (ExitState, error)
}

// ExitState is a stopped workload's last state, as the runtime recorded it.
type ExitState struct {
	Status    string // the runtime's word: "exited", "created", "dead", ...
	ExitCode  int
	OOMKilled bool
	Error     string // the runtime's own error, e.g. an OCI start failure
}

// String renders the state as one clause for a failure message.
func (e ExitState) String() string {
	var b strings.Builder

	fmt.Fprintf(&b, "state %s, exit code %d", orUnknown(e.Status), e.ExitCode)

	switch {
	case e.OOMKilled:
		b.WriteString(", killed by the kernel for exceeding its memory limit (OOMKilled)")
	case e.ExitCode == 137:
		b.WriteString(" (SIGKILL: killed from outside, or by the kernel for memory)")
	case e.ExitCode == 143:
		b.WriteString(" (SIGTERM: stopped from outside)")
	}

	if e.Status == "created" {
		b.WriteString(" - the container was created but never started")
	}

	if s := strings.TrimSpace(e.Error); s != "" {
		b.WriteString("; the runtime reported: " + hostPaths.ReplaceAllString(s, "<host path>"))
	}

	return b.String()
}

// hostPaths are the host-side paths a runtime writes into its start errors: volume data
// directories, containerd's and docker's state, an operator's home. A failure message goes to
// the API's caller, and these are the operator's; the container's own paths ("/data", "/app")
// are not under these roots and are kept.
var hostPaths = regexp.MustCompile(`/(?:var/lib|var/run|run|home|Users|root|tmp|private|snap|mnt)/[^\s"':,;]*`)

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}

	return s
}
