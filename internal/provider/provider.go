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
