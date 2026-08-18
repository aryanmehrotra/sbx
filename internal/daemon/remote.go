package daemon

// A deployed sandbox, on the local dashboard.
//
// `sbx ui` reads a provider.Provider, and everything it shows - the table, the usage columns,
// the ceilings, the traces - is built from List and the optional Meter and Limiter. So the way
// to put a sandbox that is somewhere else onto this screen is not a second dashboard: it is a
// Provider whose List is an HTTP request. The renderer never learns the difference.
//
// Wake, sleep, re-limit, remove and logs go through too, each an authed call to a control
// endpoint on the far daemon - so the dashboard's keys do over connect what they do locally.
// This is a deliberate widening of what the token is worth: it once bought reading and a tunnel,
// and now controls the deployment, so a leaked one is a larger incident than it was. Exec, a
// TTY and file copy stay refused - a shell over the tunnel is its own surface - and none of it
// does anything in front mode, where there is no runtime behind the endpoint to act on.

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

	// Where a control call goes. A ref is namespaced by deployment on the way out (see List),
	// so wake/sleep/limit/logs undo that here to reach the right endpoint with the ref it knows.
	// Remove takes a sandbox name rather than a ref, so it needs its own routing.
	route         map[string]refTarget
	sandboxSource map[string]*source
}

// refTarget is the deployment a namespaced ref belongs to, and the bare ref that deployment
// issued - which is what the far side stored it under.
type refTarget struct {
	src *source
	raw string
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
		sources:       sources,
		only:          sandbox,
		usage:         map[string]provider.Usage{},
		limits:        map[string]provider.Limits{},
		route:         map[string]refTarget{},
		sandboxSource: map[string]*source{},
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
		route  = map[string]refTarget{}
		sbxSrc = map[string]*source{}
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

			// So a control call for this ref reaches the deployment that issued it, with the
			// bare ref that deployment stored - not the namespaced one this side shows.
			route[ref] = refTarget{src: res.src, raw: f.Ref}

			// Remove routes by sandbox. First writer wins: two deployments with a sandbox of the
			// same name is the same collision that namespaces refs, and there is no ref here to
			// disambiguate on, so removing reaches whichever was listed first rather than both.
			if _, seen := sbxSrc[f.Sandbox]; !seen {
				sbxSrc[f.Sandbox] = res.src
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
	r.route, r.sandboxSource = route, sbxSrc
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

// readOnly is what the verbs that stay refused say. Exec, a TTY and file copy are not dashboard
// actions and are a larger surface than wake/sleep/limit/remove/logs - a shell over the tunnel
// is its own feature with its own risks - so they remain off here rather than being half-built.
//
// It names the endpoint rather than the machine, because the thing being refused is not "sbx
// cannot do this" - it is "not through this door". The same command works where the sandbox is.
func (r *Remote) readOnly(verb string) error {
	return fmt.Errorf("%s is not available over sbx connect: reach it where the sandbox runs, "+
		"or open a shell there", verb)
}

// targetFor resolves a ref this side handed out to the deployment that owns it and the bare ref
// that deployment stored. Unknown until List has run, and after that only for refs that were in
// the last listing.
func (r *Remote) targetFor(ref string) (refTarget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.route[ref]
	if !ok {
		return refTarget{}, fmt.Errorf("no deployment is fronting %q - it was not in the last "+
			"listing", ref)
	}

	return t, nil
}

// Start wakes a service through the control endpoint. The local dashboard wakes by dialling the
// address, which a remote dashboard cannot do without a live tunnel, so waking is asked for.
func (r *Remote) Start(ctx context.Context, ref string) error {
	t, err := r.targetFor(ref)
	if err != nil {
		return err
	}

	return control(ctx, t.src, "wake", map[string]any{"ref": t.raw})
}

func (r *Remote) Stop(ctx context.Context, ref string) error {
	t, err := r.targetFor(ref)
	if err != nil {
		return err
	}

	return control(ctx, t.src, "sleep", map[string]any{"ref": t.raw})
}

func (r *Remote) SetLimits(ctx context.Context, ref string, l provider.Limits) error {
	t, err := r.targetFor(ref)
	if err != nil {
		return err
	}

	return control(ctx, t.src, "limit", map[string]any{
		"ref": t.raw, "nano_cpus": l.NanoCPUs, "mem_bytes": l.MemBytes,
	})
}

// Remove routes by sandbox name rather than ref - see the note where sandboxSource is filled.
func (r *Remote) Remove(ctx context.Context, sandbox string) error {
	r.mu.Lock()
	src, ok := r.sandboxSource[sandbox]
	r.mu.Unlock()

	if !ok {
		return fmt.Errorf("no deployment is fronting a sandbox called %q", sandbox)
	}

	return control(ctx, src, "remove", map[string]any{"sandbox": sandbox})
}

// Logs streams a tail from the control endpoint into w. Not following: the dashboard reads a
// tail and shows it, matching what the server offers.
func (r *Remote) Logs(ctx context.Context, ref string, lines int, _ bool, w io.Writer) error {
	t, err := r.targetFor(ref)
	if err != nil {
		return err
	}

	return controlLogs(ctx, t.src, t.raw, lines, w)
}

func (r *Remote) Exec(context.Context, string, []string) (string, error) {
	return "", r.readOnly("running a command in a service")
}

func (r *Remote) ExecTTY(context.Context, string, []string) error {
	return r.readOnly("a terminal into a service")
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
