package daemon

// The daemon. It owns the ports in front of every sandbox so that nothing else has to own
// their lifecycle: a connection wakes what is behind a port, silence puts it back.
//
// The same binary is the local proxy and the in-cluster activator. Only the Provider and
// the addresses differ, because the policy - connect means wake, quiet means sleep - was
// never about containers.

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
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
	}
}

// Run drives discovery and reaping until ctx is done.
func (d *daemon) Run(ctx context.Context) { d.run(ctx) }

type daemon struct {
	provider provider.Provider
	idle     time.Duration
	ready    time.Duration
	refresh  time.Duration

	mu    sync.Mutex
	units map[string]*unit              // ref -> unit
	stop  map[string]context.CancelFunc // ref -> listener cancel
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
	if err != nil {
		return err
	}

	d := &daemon{
		provider: p,
		idle:     *idle,
		ready:    *ready,
		refresh:  *refresh,
		units:    map[string]*unit{},
		stop:     map[string]context.CancelFunc{},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Say so, in a file, so `sbx create` can tell "no daemon" from "the daemon has not
	// noticed this sandbox yet" - those need opposite advice.
	defer MarkRunning(p.Name())()

	logs.Default.Info("", "", "sbx %s · provider %s · idle %s · in-cluster %v", logs.Version, p.Name(), d.idle, InCluster())

	d.run(ctx)

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
			continue
		}

		legs := legsOf(f)
		if len(legs) == 0 {
			// A unit with nothing to front is not something to guess about: fronting the
			// wrong port would splice a caller into silence.
			logs.Default.Warn(f.Sandbox, f.Service, "no ports to front, skipping")
			continue
		}

		u := newUnit(f.Sandbox, f.Service, f.Ref, f.Ref, legs, f.Running)

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
		if !u.isAwake() || !u.sleepable(ctx, d.provider) || u.idleFor() < d.idle {
			continue
		}

		u.sleep(ctx, d.provider, d.idle)
	}
}
