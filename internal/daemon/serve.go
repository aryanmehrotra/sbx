package daemon

// The daemon. It owns the ports in front of every sandbox so that nothing else has to own
// their lifecycle: a connection wakes what is behind a port, silence puts it back.
//
// The same binary is the local proxy and the in-cluster activator. Only the Provider and
// the addresses differ, because the policy - connect means wake, quiet means sleep - was
// never about containers.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"strings"
)

// envOr is here rather than in main because the daemon is the only thing that reads
// configuration from the environment, and a flag default is the wrong place to discover
// that a variable existed.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

type errNotReady struct {
	name    string
	timeout time.Duration
}

func (e errNotReady) Error() string {
	return fmt.Sprintf("%s did not answer within %s", e.name, e.timeout)
}

func itoa(i int) string { return strconv.Itoa(i) }

// inCluster reports whether this process is a pod. Kubernetes always sets this.
func InCluster() bool { return os.Getenv("KUBERNETES_SERVICE_HOST") != "" }

// knock opens and immediately closes a connection, purely to ask for a wake.
func Knock(port int) {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 2*time.Second)
	if err == nil {
		_ = c.Close()
	}
}

// Reachable reports whether anything accepts on a port.
//
// Knock deliberately discards its error - it is a wake signal, and a sandbox that is asleep
// with no daemon in front is not an error condition to knock on. This is the other question:
// can the address a caller was handed actually be connected to.
func Reachable(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 2*time.Second)
	if err != nil {
		return false
	}

	_ = c.Close()

	return true
}

// New builds a daemon that can be run in-process. Selftest uses it: the honest way to test
// the wake path is to run the real one, not a copy of its logic.
func New(p provider.Provider, idle, ready, refresh time.Duration) *daemon {
	return &daemon{
		provider: p,
		idle:     idle,
		ready:    ready,
		refresh:  refresh,
		units:    map[string]*unit{},
		stop:     map[string]context.CancelFunc{},
		egress:   map[string]*egressProxy{},
	}
}

// Run drives discovery and reaping until ctx is done.
func (d *daemon) Run(ctx context.Context) { d.run(ctx) }

type daemon struct {
	provider provider.Provider

	// startupErr is why there is no provider, when there is none. Set only where the connect
	// endpoint was asked for, because that is the only case where staying up beats exiting.
	startupErr error

	// fronted are ports this process carries without discovering them - see --front.
	fronted map[int]fronted
	idle    time.Duration
	ready   time.Duration
	refresh time.Duration

	mu     sync.Mutex
	units  map[string]*unit              // ref -> unit
	stop   map[string]context.CancelFunc // ref -> listener cancel
	egress map[string]*egressProxy       // bridge gateway -> running egress filter
}

// runServe is the daemon. One per machine, or one Deployment per cluster namespace: it
// fronts every sandbox's ports, so a second copy would fight the first for the listeners.
func Serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	kind := fs.String("provider", envOr("SBX_PROVIDER_KIND", "docker"), "docker | kubernetes")
	socket := fs.String("socket", "", "docker endpoint; defaults to DOCKER_HOST, then the active docker context")
	namespace := fs.String("namespace", envOr("SBX_NAMESPACE", "sbx"), "kubernetes namespace")
	idle := fs.Duration("idle", 5*time.Minute, "sleep a service after this long with no bytes")
	ready := fs.Duration("ready", 90*time.Second, "give up waking a service after this long")
	refresh := fs.Duration("refresh", 15*time.Second, "how often to look for new or removed sandboxes")

	// Off unless asked for, and never inferred from the environment.
	//
	// An earlier draft defaulted this to :$PORT because that is what a PaaS provides. PORT is
	// exported in a great many developer shells, so that default would have opened a network
	// listener on a laptop by accident - or, with no token set, refused to start a daemon that
	// had worked yesterday. A deployment passes `--connect-addr :$PORT` itself, which is one
	// word in a manifest and cannot surprise anybody.
	connectAddr := fs.String("connect-addr", envOr("SBX_CONNECT_ADDR", ""), "serve the tunnel endpoint here (needs SBX_CONNECT_TOKEN); off unless set")

	// Carrying a port beside a workload, rather than managing anything.
	//
	// This is sbx in a container next to a database, on a platform that gives one HTTP port
	// and no container runtime. There is nothing to discover and nothing to wake - the
	// platform woke the container - so these ports skip the provider entirely and are the
	// only thing the tunnel will carry.
	front := fs.String("front", envOr("SBX_FRONT", ""), "carry these local ports over the connect endpoint, e.g. 5432 or db=5432,cache=6379")
	behindProxy := fs.Bool("behind-proxy", false, "something in front of this terminates TLS, so a non-loopback address is safe")
	_ = fs.Parse(args)

	// One per machine. A second copy binds nothing - every listener fails with "address
	// already in use", logged once per port with no retry - while the process stays up
	// looking healthy, and on exit it removes the first daemon's presence record. That is a
	// normal accident: a supervised unit from deploy/ plus a manual `sbx serve &`, or two
	// terminal tabs.
	if running, ok := Running(); ok && running.PID != os.Getpid() {
		return fmt.Errorf("sbx serve is already running (pid %d, since %s). One per machine - "+
			"it fronts every sandbox's ports.\n     Stop that one first, or leave it: it is "+
			"already serving everything this would.",
			running.PID, running.Since.Format("15:04:05"))
	}

	p, err := provider.For(*kind, *socket, *namespace)

	// A daemon that cannot reach its runtime exits - unless it was asked to serve the connect
	// endpoint, in which case it starts anyway and says so there. With --front there is nothing
	// it needed the runtime for in the first place.
	//
	// The two cases genuinely differ. On a laptop, failing fast is right: you typed a command,
	// you get the reason, you fix it. Deployed, exiting is the worst thing it can do - the
	// process vanishes, the platform's scale-to-zero never completes a wake, and the operator
	// gets a holding page forever with nothing to read. Measured on one managed platform: the deploy
	// reported active and the endpoint served the platform's "Starting up..." page for four
	// minutes, because there was nothing listening to say otherwise.
	//
	// So: listen, answer, and be honest about being useless. `/healthz` still answers because
	// the process IS alive - that is what liveness means - and `/v1/fleet` carries the reason.
	if err != nil && *connectAddr == "" {
		return err
	}

	fronts, ferr := parseFront(*front)
	if ferr != nil {
		return ferr
	}

	if len(fronts) > 0 && *connectAddr == "" {
		return errors.New("--front only means something with --connect-addr: it names what the " +
			"tunnel may carry, and without the tunnel nothing can ask for it")
	}

	var startupErr error

	if err != nil {
		startupErr = err

		logs.Default.Info("", "", "no container runtime: %v", err)
		logs.Default.Info("", "", "serving the connect endpoint anyway so the reason is reachable")
	}

	d := &daemon{
		provider:   p,
		startupErr: startupErr,
		fronted:    fronts,
		idle:       *idle,
		ready:      *ready,
		refresh:    *refresh,
		units:      map[string]*unit{},
		stop:       map[string]context.CancelFunc{},
		egress:     map[string]*egressProxy{},
	}

	var connectSrv *http.Server

	if *connectAddr != "" {
		connectSrv, err = d.Connect(ConnectOptions{
			Addr:        *connectAddr,
			Token:       os.Getenv("SBX_CONNECT_TOKEN"),
			BehindProxy: *behindProxy,
		})
		if err != nil {
			return err
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Say so, in a file, so `sbx create` can tell "no daemon" from "the daemon has not
	// noticed this sandbox yet" - those need opposite advice.
	// Named even when there is no provider to ask: this path exists precisely because p is nil,
	// and a presence record that panics is worse than one that says "unknown".
	name := "none"
	if p != nil {
		name = p.Name()
	}

	defer MarkRunning(name)()

	logs.Default.Info("", "", "sbx %s · provider %s · idle %s · in-cluster %v", logs.Version, name, d.idle, InCluster())

	if connectSrv != nil {
		logs.Default.Info("", "", "connect endpoint on %s", connectSrv.Addr)

		go func() {
			if err := connectSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logs.Default.Info("", "", "connect endpoint stopped: %v", err)
			}
		}()

		defer func() { _ = connectSrv.Close() }()
	}

	if startupErr == nil {
		d.run(ctx)
	} else {
		// Nothing to discover and nothing to reap; just hold the endpoint open so the reason
		// stays readable until somebody fixes the deployment.
		<-ctx.Done()
	}

	// Deliberately does not sleep anything on the way out. The daemon dying is not a reason
	// to tear down a database somebody is mid-migration on.
	logs.Default.Info("", "", "stopped - sandboxes left as they are")

	return nil
}

func (d *daemon) run(ctx context.Context) {
	d.discover(ctx)

	discovery := time.NewTicker(d.refresh)
	defer discovery.Stop()

	// The reaper's cadence has to follow the idle window, not ignore it. Fixed at 30s, a
	// sandbox configured to sleep after 5s slept after 60 - two ticks, because the first
	// one only established that it had ever been serving. The setting meant nothing below
	// half a minute, which is exactly where anyone testing it would set it.
	reap := time.NewTicker(reapEvery(d.idle))
	defer reap.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-discovery.C:
			d.discover(ctx)
		case <-reap.C:
			d.reap(ctx)
		}
	}
}

// discover reconciles the live sandbox set with the units being served. New sandboxes get
// listeners; removed ones get theirs closed.
func (d *daemon) discover(ctx context.Context) {
	found, err := d.provider.List(ctx, "")
	if err != nil {
		logs.Default.Error("", "", "discovery failed: %v", err)
		return
	}

	// Reserve the label column before anything below logs into it.
	//
	// Without this the daemon printed every line with an unpadded label, so the message
	// started wherever the sandbox and service names happened to end and the left edge was
	// ragged for the whole run. `sbx logs` was the only caller of Align, which is why it
	// looked fine there and nowhere else.
	width := 0
	for _, f := range found {
		if w := len(f.Sandbox) + 1 + len(f.Service); w > width {
			width = w
		}
	}

	logs.Default.Align(width)

	seen := map[string]bool{}

	for _, f := range found {
		seen[f.Ref] = true

		d.mu.Lock()
		_, known := d.units[f.Ref]
		d.mu.Unlock()

		if known {
			d.correctAwake(f)

			continue
		}

		legs := legsOf(f)
		if len(legs) == 0 {
			// A unit with nothing to front is not something to guess about: fronting the
			// wrong port would splice a caller into silence.
			logs.Default.Warn(f.Sandbox, f.Service, "no ports to front, skipping")
			continue
		}

		u := newUnit(f.Sandbox, f.Service, f.Ref, f.Instance, f.Ref, legs, f.Running)
		u.keepAwake, u.idle = idlePolicy(f.Idle)
		u.dependsOn = f.DependsOn
		u.peers = d.peersOf

		uctx, ucancel := context.WithCancel(ctx)

		d.mu.Lock()
		d.units[f.Ref] = u
		d.stop[f.Ref] = ucancel
		d.mu.Unlock()

		for _, l := range legs {
			go func(u *unit, ref string, l leg) {
				err := u.serve(uctx, d.provider, l, d.ready)
				if err == nil || uctx.Err() != nil {
					return // shutting down, which is not a failure
				}

				logs.Default.Error(u.sandbox, u.service, "stopped serving :%d: %v", l.Listen, err)

				// Forget it, so the next discovery tick tries again.
				//
				// Without this the unit stays in d.units, discover() skips it forever as
				// already known, and the sandbox is unreachable for the life of a daemon that
				// is designed to run for weeks. The way in is ordinary: `sbx rm x` then a new
				// sandbox moments later reuses the freed slot, and its bind can land while the
				// old listener is still closing. One "address already in use" and that
				// sandbox was finished.
				d.forget(ref, u)
			}(u, f.Ref, l)
		}
	}

	d.mu.Lock()
	for ref, u := range d.units {
		if seen[ref] {
			continue
		}

		logs.Default.Info(u.sandbox, u.service, "gone")
		d.stop[ref]()
		delete(d.units, ref)
		delete(d.stop, ref)
	}
	d.mu.Unlock()

	// A sandbox with an egress allow-list gets a filtering proxy on its bridge gateway,
	// started here and torn down when the sandbox goes. Off the wake path.
	d.reconcileEgress(found)
}

// correctAwake revokes a belief the provider contradicts.
//
// wake() returns immediately for a unit it thinks is already up, and that optimism is
// deliberate - the alternative is a probe on every connection to a service that is almost
// always running. For the unit a connection addressed it is also self-correcting: the dial
// straight afterwards fails, and handle() revokes and wakes properly.
//
// A dependency has no such correction. It is woken so that something else can reach it over
// the sandbox network, so nothing dials it from here, and a container stopped out of band -
// by `docker stop`, by a crash, by a docker daemon restart - would stay believed-awake
// indefinitely while being absent from the network's DNS. That is exactly the failure
// dependency-ordered wake exists to prevent, arrived at from the other side.
//
// One direction only. Marking a unit awake is wake()'s job, done under the lock that
// serialises it against sleep; a discovery tick asserting it from the side would make the
// word mean nothing. And a wake in flight is left alone: TryLock rather than Lock, because
// this runs on the discovery goroutine and must not wait behind a container start.
func (d *daemon) correctAwake(f provider.Unit) {
	if f.Running {
		return
	}

	d.mu.Lock()
	u := d.units[f.Ref]
	d.mu.Unlock()

	if u == nil || !u.isAwake() {
		return
	}

	if !u.waking.TryLock() {
		return // a wake is in progress; its own bookkeeping is the truth
	}
	defer u.waking.Unlock()

	// Re-checked under the lock: a wake may have finished between the test above and here.
	if !u.isAwake() {
		return
	}

	u.setAwake(false)
	logs.Default.Info(u.sandbox, u.service, "was stopped outside sbx; will be started on demand")
}

// peersOf resolves service names to units within one sandbox.
//
// Within one, deliberately: a service name is unique inside a sandbox and nowhere else, so
// resolving across them would wake a stranger's database because it is also called mysql.
// A name that resolves to nothing is skipped rather than erroring - a dependency on a service
// that is optional, or not created, should not make the sandbox unwakeable.
func (d *daemon) peersOf(sandbox string, services []string) []*unit {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]*unit, 0, len(services))

	for _, want := range services {
		for _, u := range d.units {
			if u.sandbox == sandbox && u.service == want {
				out = append(out, u)

				break
			}
		}
	}

	return out
}

// forget drops a unit so the next discovery tick can rebuild it.
//
// It drops the unit only if `want` is still the registered one. A unit has one goroutine per
// leg, they can fail together, and a later tick may already have rebuilt the unit by the time
// the second one gets here - without this check that second goroutine would delete a healthy
// replacement it knows nothing about, and the sandbox would go dark for a tick for no reason.
func (d *daemon) forget(ref string, want *unit) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.units[ref] != want {
		return
	}

	if cancel, ok := d.stop[ref]; ok {
		cancel()
	}

	delete(d.units, ref)
	delete(d.stop, ref)
}

// reapEvery keeps the check frequent enough that the idle window is honoured and rare
// enough that a hundred sleeping sandboxes are not polled constantly.
func reapEvery(idle time.Duration) time.Duration {
	every := idle / 3

	if every < time.Second {
		every = time.Second
	}

	if every > 30*time.Second {
		every = 30 * time.Second
	}

	return every
}

func legsOf(u provider.Unit) []leg {
	n := min(len(u.Listen), len(u.Upstream))

	legs := make([]leg, 0, n)
	for i := range n {
		legs = append(legs, leg{Listen: u.Listen[i], Upstream: u.Upstream[i]})
	}

	return legs
}

// reap sleeps every unit that has been quiet for longer than the idle window.
func (d *daemon) reap(ctx context.Context) {
	d.mu.Lock()
	units := make([]*unit, 0, len(d.units))

	for _, u := range d.units {
		units = append(units, u)
	}
	d.mu.Unlock()

	for _, u := range units {
		if u.keepAwake {
			continue // an agent may be working inside it with no traffic through the proxy
		}

		window := d.idle
		if u.idle > 0 {
			window = u.idle
		}

		if !u.isAwake() || !u.sleepable(ctx, d.provider) || u.idleFor() < window {
			continue
		}

		u.sleep(ctx, d.provider, window)
	}
}

// parseFront reads --front: "5432", "5432,6379", or "db=5432,cache=6379".
//
// A name is optional and only ever cosmetic - it is what the fleet listing calls the port, so
// that somebody running two of these can tell them apart. The port is the part that matters,
// because it is the allow-list: the tunnel carries these and refuses everything else, which is
// what keeps a connect endpoint from being an open proxy into whatever the container can reach.
func parseFront(spec string) (map[int]fronted, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}

	out := map[int]fronted{}

	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		name, portText, named := strings.Cut(part, "=")
		if !named {
			name, portText = "", part
		}

		port, err := strconv.Atoi(strings.TrimSpace(portText))
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("--front %q: %q is not a port", spec, portText)
		}

		if name = strings.TrimSpace(name); name == "" {
			name = "port-" + portText
		}

		out[port] = fronted{name: name, port: port}
	}

	return out, nil
}
