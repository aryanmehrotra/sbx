package provider

// A provider is somewhere sandboxes can exist: this laptop's Docker, a cluster, later a
// pool of microVMs. The seam is here because two things about a sandbox are not portable,
// and everything else is.
//
// **Addressing is not portable.** Locally every sandbox shares one loopback, so services
// get remapped into a per-slot port block. In a cluster every pod has its own address, so
// MySQL is simply :3306 on a name — port arithmetic is a workaround for a constraint that
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
	// not at the platform's — the difference between the two was 98% of a wake.
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
// stubs that return errors — a method on the core interface is a promise that every provider
// keeps, and four methods that only docker implements is not an interface, it is a docker
// client with a kubernetes-shaped hole in it.
//
// These are optional interfaces instead. A provider implements one if it can do the thing
// natively; the CLI asks with a type assertion and reports a single clear refusal if not.
// It is the same negotiation --isolation already uses: declare what you want, and be told
// plainly when this backend cannot give it to you.
//
// The rule for adding one: the capability is named for what the USER wants, never for how a
// backend happens to do it. Snapshotter, not Committer — because the kubernetes answer is a
// volume snapshot through its own CSI, not `docker commit`, and an interface named after
// docker's verb would have made that implementation look like a workaround.

// Snapshotter saves and restores a service's state. Filesystem state — memory and running
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
	// belonging to a live sandbox, asleep or awake — asleep is the normal state here.
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
