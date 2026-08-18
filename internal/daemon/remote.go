package daemon

// A deployed sandbox, on the local dashboard.
//
// `sbx ui` reads a provider.Provider, and everything it shows - the table, the usage columns,
// the ceilings, the traces - is built from List and the optional Meter and Limiter. So the way
// to put a sandbox that is somewhere else onto this screen is not a second dashboard: it is a
// Provider whose List is an HTTP request. The renderer never learns the difference.
//
// It is read-only, and that is a decision rather than an omission. The token on a connect
// endpoint currently buys two things: read what is fronted, and carry bytes to a port. Waking,
// sleeping, capping and removing are none of those, and a token that leaks is a very different
// incident if it can also destroy a sandbox's volume. Every verb that changes something refuses
// here and says why, so the dashboard's keys report a policy instead of failing obscurely.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// Remote is a read-only Provider backed by one or more deployed sbx endpoints.
type Remote struct {
	sources []*source
	only    []string

	// What the last List brought back, keyed by the ref this type hands out.
	//
	// The dashboard calls List and then Stats on every tick, in that order, so answering Stats
	// from the listing costs one round trip per refresh rather than two. Sampling separately
	// would also mean the numbers on a row came from a different moment than the row did.
	mu     sync.Mutex
	usage  map[string]provider.Usage
	limits map[string]provider.Limits
}

// NewRemote resolves the endpoints and returns a Provider that reads them.
//
// The same resolution `sbx connect` does, deliberately: the URL checks, the refusal to send a
// token over cleartext to another machine, the per-deployment SBX_CONNECT_TOKEN_<NAME> lookup
// and the two-labels-one-variable collision are all things a dashboard needs to get right for
// exactly the same reasons a tunnel does.
func NewRemote(endpoints []Endpoint, sandbox []string) (*Remote, error) {
	sources, err := resolve(ClientOptions{Endpoints: endpoints, Sandbox: sandbox})
	if err != nil {
		return nil, err
	}

	return &Remote{
		sources: sources,
		only:    sandbox,
		usage:   map[string]provider.Usage{},
		limits:  map[string]provider.Limits{},
	}, nil
}

func (r *Remote) Name() string {
	labels := make([]string, 0, len(r.sources))
	for _, s := range r.sources {
		labels = append(labels, s.label)
	}

	return "connect " + strings.Join(labels, " ")
}

// List asks every deployment what it is fronting, at the same time.
//
// At the same time because a deployment wakes on its first request: asking three in turn pays a
// cold start three times, which is the same reasoning as the fleet fetch in `sbx connect`.
//
// One deployment failing does not fail the listing here, which is the opposite of what connect
// does - and for a reason. A port map with a hole in it sends somebody's psql to whatever else
// answers on that port, so connect refuses the lot. A dashboard showing three deployments of
// four is just a dashboard missing a row, and blanking the screen because one endpoint is
// redeploying is worse than showing what is still there.
func (r *Remote) List(ctx context.Context, sandbox string) ([]provider.Unit, error) {
	type result struct {
		src *source
		svc []fleetService
		err error
	}

	results := make([]result, len(r.sources))

	var wg sync.WaitGroup

	for i, s := range r.sources {
		wg.Add(1)

		go func() {
			defer wg.Done()

			svc, err := fetchFleet(ctx, s.base, s.token, true)
			results[i] = result{src: s, svc: svc, err: err}
		}()
	}

	wg.Wait()

	var (
		units  []provider.Unit
		usage  = map[string]provider.Usage{}
		limits = map[string]provider.Limits{}
		errs   []error
	)

	for _, res := range results {
		if res.err != nil {
			errs = append(errs, res.err)

			continue
		}

		for _, f := range chooseServices(res.svc, r.only) {
			if sandbox != "" && f.Sandbox != sandbox {
				continue
			}

			ref := f.Ref

			// Two deployments can each hold a service called "db", and a ref is only unique
			// within the daemon that issued it. The dashboard keys its history and its limits
			// by ref, so colliding refs would draw one service's trace under another's name.
			if len(r.sources) > 1 {
				ref = res.src.label + "/" + f.Ref
			}

			client := make([]provider.Endpoint, 0, len(f.Ports))
			for _, p := range f.Ports {
				client = append(client, provider.Endpoint{Host: "127.0.0.1", Port: p})
			}

			units = append(units, provider.Unit{
				Sandbox:  f.Sandbox,
				Service:  f.Service,
				Ref:      ref,
				Instance: f.Instance,
				Running:  f.Awake,
				Client:   client,
			})

			if u := f.Usage; u != nil {
				usage[ref] = provider.Usage{
					CPUNanos: u.CPUNanos, SystemNanos: u.SystemNanos, OnlineCPUs: u.OnlineCPUs,
					MemBytes: u.MemBytes, MemLimit: u.MemLimit,
				}
			}

			if l := f.Limits; l != nil {
				limits[ref] = provider.Limits{NanoCPUs: l.NanoCPUs, MemBytes: l.MemBytes}
			}
		}
	}

	r.mu.Lock()
	r.usage, r.limits = usage, limits
	r.mu.Unlock()

	// Only where nothing at all came back. Something on screen and a missing deployment is a
	// better answer than an empty screen and an error.
	if len(units) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return units, nil
}

// Stats answers from the last listing - see the note on the cache.
func (r *Remote) Stats(_ context.Context, refs []string) (map[string]provider.Usage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string]provider.Usage, len(refs))

	for _, ref := range refs {
		if u, ok := r.usage[ref]; ok {
			out[ref] = u
		}
	}

	return out, nil
}

// Limits likewise. A deployment whose backend cannot report them sends nothing, and an absent
// entry here is the zero Limits - which the dashboard already draws as "no ceiling".
func (r *Remote) Limits(_ context.Context, ref string) (provider.Limits, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.limits[ref], nil
}

// readOnly is what every verb that would change something says.
//
// It names the endpoint rather than the machine, because the thing being refused is not "sbx
// cannot do this" - it is "not through this door". The same command works where the sandbox is.
func (r *Remote) readOnly(verb string) error {
	return fmt.Errorf("%s is not available over sbx connect: a connect endpoint carries bytes "+
		"and reports what it is fronting, and nothing else. Run it where the sandbox is, or "+
		"open a shell there", verb)
}

func (r *Remote) Start(context.Context, string) error { return r.readOnly("waking a service") }
func (r *Remote) Stop(context.Context, string) error  { return r.readOnly("sleeping a service") }

func (r *Remote) Remove(context.Context, string) error {
	return r.readOnly("removing a sandbox")
}

func (r *Remote) SetLimits(context.Context, string, provider.Limits) error {
	return r.readOnly("setting a limit")
}

func (r *Remote) Exec(context.Context, string, []string) (string, error) {
	return "", r.readOnly("running a command in a service")
}

func (r *Remote) ExecTTY(context.Context, string, []string) error {
	return r.readOnly("a terminal into a service")
}

func (r *Remote) Logs(context.Context, string, int, bool, io.Writer) error {
	return r.readOnly("reading logs")
}

func (r *Remote) Copy(context.Context, string, string, string) error {
	return r.readOnly("copying a file")
}

func (r *Remote) Create(context.Context, string, int, int, string, spec.Service,
	[]provider.Endpoint, string, provider.Isolation) error {
	return r.readOnly("creating a service")
}

// Healthy and Probe report what the listing said, which is the only opinion this side has.
// declared=false is the honest answer: the awake flag is the remote daemon's wake state rather
// than a health check run just now, and every caller treats "not declared" as worth saying.
func (r *Remote) Healthy(_ context.Context, ref string) (bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, running := r.usage[ref]

	return running, false
}

func (r *Remote) Probe(ctx context.Context, ref string) (bool, bool) { return r.Healthy(ctx, ref) }

// Endpoints and AllocSlot exist for the create path, which is refused above.
func (r *Remote) Endpoints(string, string, int, int, []int) []provider.Endpoint { return nil }

func (r *Remote) AllocSlot(context.Context, string) (int, error) {
	return 0, r.readOnly("allocating a port slot")
}
