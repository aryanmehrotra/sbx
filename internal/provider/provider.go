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
	"fmt"
	"io"
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
}

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
)

func (i Isolation) Valid() bool {
	switch i {
	case IsolationContainer, IsolationGVisor, IsolationKata:
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
	default:
		return nil, fmt.Errorf("unknown provider %q (want docker or kubernetes)", kind)
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
	// Commit saves a unit's filesystem as a named image.
	Commit(ctx context.Context, ref, image string) error

	// Images lists saved images beginning with prefix.
	Images(ctx context.Context, prefix string) ([]string, error)

	// CopyVolume replicates one volume into another, creating the destination.
	CopyVolume(ctx context.Context, src, dst string) error

	// VolumeFor names the volume a service's data lives in.
	VolumeFor(sandbox, service string) string
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
