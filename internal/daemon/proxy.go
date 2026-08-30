package daemon

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// unit is one wakeable service and the ports that front it.
//
// One service, not one sandbox: a worktree's MySQL and its ClickHouse sleep and wake
// independently, so the analytics store a branch never queries costs nothing at all.
//
// Nothing here knows what a container is. It binds a port, asks a Provider to start
// something, waits for that thing to say it is serving, and moves bytes - which is the same
// job on a laptop and in a cluster, and the reason the activator is this file plus a
// different Provider rather than a second program.
type unit struct {
	sandbox string
	service string // for logs: which part of the sandbox this is
	ref     string // provider handle: container id, or deployment name

	// instance changes when the thing behind ref is replaced and ref is not - see
	// provider.Unit.Instance. A tunnel holding a port across `sbx rm x && sbx create x` would
	// otherwise carry on addressing a service that no longer exists.
	instance string
	name     string // for logs
	legs     []leg

	// dependsOn names the services in this sandbox that must be serving before this one is
	// started, read from the sbx.dependsOn label. Empty for anything that declared nothing,
	// which is every single-service sandbox and the whole of the published wake numbers.
	dependsOn []string

	// peers resolves those names to units. The daemon owns the registry, so the resolver is
	// handed in rather than the unit reaching for a global - which also lets a test build a
	// dependency graph without a daemon loop running.
	peers func(sandbox string, services []string) []*unit

	// lastByte is the only activity signal. Connection counting is not enough: a Go
	// service's pool holds idle connections open indefinitely, so a unit fronted by a
	// running service would never sleep. Bytes are what "in use" actually means.
	lastByte atomic.Int64

	mu     sync.Mutex
	live   map[net.Conn]struct{} // open client conns, closed on sleep
	awake  bool
	waking sync.Mutex // serialises wake so a burst of connections starts the workload once

	// egressGateway is the bridge gateway this unit's egress filter listens on, or "".
	//
	// Carried so the filter can stamp activity back onto the unit. It is the missing half of
	// the idle signal for a box nothing dials: an agent working inside one sends nothing
	// through the proxy, so the only evidence it is busy is what it sends OUT - and for an
	// allow-listed box that leaves through code sbx already owns.
	egressGateway string

	// keepAwake and idle are the per-service override of the global idle window: keepAwake
	// means never auto-sleep (an agent working inside a box sends nothing through the proxy),
	// and idle > 0 is a longer window than the daemon's default. See spec.Service.Idle.
	keepAwake bool
	idle      time.Duration

	// served records that this unit has been seen serving at least once.
	//
	// Until then it is not eligible to sleep, because "idle" is meaningless before a
	// service has ever been up: a sandbox that is still pulling an image and running its
	// schema migrations looks exactly like one nobody has touched. Measured from discovery
	// instead, a 39-second creation was put to sleep underneath the command creating it.
	served bool
}

// leg is one port this daemon listens on and where it forwards.
type leg struct {
	Listen   int               // bound locally by the daemon
	Upstream provider.Endpoint // dialled once the workload is serving

	// addr is Upstream resolved once, and dialled directly on every connection after that.
	//
	// The upstream of a leg never moves - it is a fixed host and port for the life of the
	// unit - but every connection used to re-format it with fmt.Sprintf and hand the string to
	// net.DialTimeout, which parsed it back and walked the resolver. Profiled: that dial was
	// 27 of the 38 allocations a connection costs and 87% of handle()'s CPU. Resolving once
	// removes the formatting and the parsing from the path a psql or a redis-cli pays.
	//
	// nil where the address is not a plain IP - a cluster Service name has to be resolved on
	// each dial, because that is the point of it - and the dial falls back to the old path.
	addr *net.TCPAddr
}

// resolve fills in addr where the upstream is a literal address, which is every leg on the
// docker provider: the daemon forwards to 127.0.0.1:<backing port>.
//
// Deliberately no DNS here. A name that has to be looked up is one whose answer can change,
// and caching that would be a bug rather than an optimisation - so a name is left to the
// dialler and only a literal is pinned.
func (l *leg) resolve() {
	ip := net.ParseIP(l.Upstream.Host)

	// Loopback, not merely a literal.
	//
	// The fast path drops the dial timeout, and the argument for that is specific to loopback:
	// there is no network in front of it, so connect() either completes or returns
	// ECONNREFUSED. It does not hold one hop further out - a listener whose accept backlog is
	// full drops the SYN silently, and the kernel then retries for tcp_syn_retries, which is
	// minutes. An unbounded dial there would be worse than the allocations it saves, and an
	// agent opening hundreds of short connections is exactly the workload that fills a backlog.
	//
	// Every address the docker provider emits is 127.0.0.1, so this gives up nothing today and
	// keeps a future provider that publishes on a LAN address on the bounded path.
	if ip == nil || !ip.IsLoopback() {
		return
	}

	l.addr = &net.TCPAddr{IP: ip, Port: l.Upstream.Port}
}

// idlePolicy turns a service's idle override into a keep-awake flag and a window. "" uses the
// global default; "never" or "0" never auto-sleeps; a duration is a longer window. The value was
// validated at spec load, so a parse error here just falls back to the global.
func idlePolicy(s string) (keepAwake bool, idle time.Duration) {
	switch s {
	case "":
		return false, 0
	case "never", "0":
		return true, 0
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return false, 0
	}

	return false, d
}

func newUnit(sandbox, service, ref, instance, name string, legs []leg, running bool) *unit {
	u := &unit{
		sandbox:  sandbox,
		instance: instance,
		service:  service,
		ref:      ref,
		name:     name,
		legs:     legs,
		live:     map[net.Conn]struct{}{},
		awake:    running,
	}
	u.touch()

	return u
}

func (u *unit) touch() { u.lastByte.Store(time.Now().UnixNano()) }

// wakeDeps brings this unit's declared dependencies up, concurrently, before it starts.
//
// Concurrent because siblings are independent by definition - config and discoverer both
// needing mysql does not make them need each other - so the cost of a layer is its slowest
// member rather than its sum. Recursion walks the chain, and the visited set both dedupes a
// diamond and stops a cycle: two services declaring each other is a spec written by hand, and
// a daemon that hangs holding the connection open is a worse answer than starting both.
func (u *unit) wakeDeps(ctx context.Context, p provider.Provider, readyTimeout time.Duration) error {
	if len(u.dependsOn) == 0 || u.peers == nil {
		return nil
	}

	return wakeAll(ctx, p, readyTimeout, u.peers(u.sandbox, u.dependsOn), newVisited(u.name))
}

// visited is the set of units a single wake has already claimed, shared across the whole
// recursion so a diamond is woken once and a cycle terminates.
//
// It carries its own lock because the recursion BRANCHES. Siblings are woken in parallel and
// each descends into wakeAll with the same set, so two nested calls read and write it at the
// same moment. It was a bare map, and the only mutex in wakeAll is a fresh local one per
// invocation - which guards `first`, and cannot guard something shared between invocations.
// Go detects a concurrent map write and calls fatal(), which no recover can catch: the daemon
// dies, taking every sandbox's ports with it.
//
// It needs a graph that branches into further dependencies to happen at all - gateway on api
// and worker, each with a datastore - so a linear chain never showed it, and a fourteen-service
// stack is exactly the shape that does.
type visited struct {
	mu sync.Mutex
	m  map[string]bool
}

func newVisited(names ...string) *visited {
	v := &visited{m: make(map[string]bool, len(names)+8)}
	for _, n := range names {
		v.m[n] = true
	}

	return v
}

// claim reports whether this call is the one that took the name, so exactly one goroutine
// wakes a given unit however many dependents ask for it at once.
func (v *visited) claim(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.m[name] {
		return false
	}

	v.m[name] = true

	return true
}

// wakeAll wakes a set of units in parallel and waits for all of them.
//
// A failure is returned but does not cancel the siblings: they are already starting, and
// stopping halfway would leave the sandbox in a state nobody asked for.
func wakeAll(ctx context.Context, p provider.Provider, readyTimeout time.Duration, us []*unit, seen *visited) error {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		first error
		next  []*unit
	)

	for _, d := range us {
		if !seen.claim(d.name) {
			continue
		}

		next = append(next, d)
	}

	for _, d := range next {
		wg.Add(1)

		go func(d *unit) {
			defer wg.Done()

			// Its own dependencies first, sharing the visited set so a diamond is woken once.
			if d.peers != nil && len(d.dependsOn) > 0 {
				mu.Lock()
				deeper := d.peers(d.sandbox, d.dependsOn)
				mu.Unlock()

				if err := wakeAll(ctx, p, readyTimeout, deeper, seen); err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()

					return
				}
			}

			if err := d.wakeSelf(ctx, p, readyTimeout); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(d)
	}

	wg.Wait()

	return first
}

// sleepable reports whether the idle clock should count for this unit yet.
//
// The first time it is seen serving, the clock is restarted from that moment: a unit that
// took two minutes to come up has not been idle for two minutes, it has been starting.
func (u *unit) sleepable(ctx context.Context, p provider.Provider) bool {
	u.mu.Lock()
	served := u.served
	u.mu.Unlock()

	if served {
		return true
	}

	if serving, declared := p.Healthy(ctx, u.ref); !serving && declared {
		return false
	}

	u.mu.Lock()
	u.served = true
	u.mu.Unlock()
	u.touch()

	return false
}

func (u *unit) idleFor() time.Duration {
	return time.Since(time.Unix(0, u.lastByte.Load()))
}

// serve accepts on one port for the lifetime of ctx.
func (u *unit) serve(ctx context.Context, p provider.Provider, l leg, readyTimeout time.Duration) error {
	// Port 0 is the one value that fails by succeeding. net.Listen treats it as "give me any
	// free port", so a leg carrying it would bind a random one, log that it is listening, and
	// front the service at an address nothing will ever dial - while `sbx env` hands out 0.
	//
	// It cannot come from sbx, which allocates from a fixed block, only from a ports label that
	// was corrupted or hand-edited. ParsePorts deliberately does not reject those: a label it
	// refuses makes the container invisible to `sbx list` AND to `sbx rm`, which is a sandbox
	// nobody can clean up. So the value is allowed through discovery, stays visible and
	// removable, and is refused here - at the one place where acting on it would be silent.
	if l.Listen < 1 || l.Listen > 65535 {
		return fmt.Errorf("%s: port %d cannot be listened on - its sbx.ports label is wrong; "+
			"the sandbox is still listed and can be removed", u.name, l.Listen)
	}

	ln, err := net.Listen("tcp", listenAddr(l.Listen))
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	logs.Default.Info(u.sandbox, u.service, "listening on :%d, forwards to %s", l.Listen, l.Upstream)

	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}

		go u.handle(ctx, p, c, l, readyTimeout)
	}
}

func (u *unit) handle(ctx context.Context, p provider.Provider, client net.Conn, l leg, readyTimeout time.Duration) {
	defer client.Close()

	u.touch()

	// Held by default, which is the whole design: the client waits and its request is served,
	// and nothing on the other end has to know sbx exists. A browser is the one caller that
	// reads a held connection as a broken link, so when the gate is on and the wait runs long
	// enough to be worth explaining, an HTTP request gets a page saying what is happening.
	// Anything that is not HTTP, and any wake fast enough not to need it, is untouched.
	if pre, served := u.maybeWaitingPage(ctx, p, client, readyTimeout); served {
		return
	} else if len(pre) > 0 {
		// Whatever was read while deciding is replayed first, or the service receives a
		// request with its verb missing.
		client = &prefixConn{Conn: client, pre: pre}
	}

	if err := u.wake(ctx, p, readyTimeout); err != nil {
		// Hanging up is the honest failure. A caller that gets a closed connection retries
		// or reports; one that gets a silently empty stream does neither.
		logs.Default.Event(logs.LevelError, u.sandbox, u.service, "wakeFailed", 0, "could not wake: %v", err)
		return
	}

	upstream, err := l.dial()
	if err != nil {
		// This is where the fast path in wake() gets corrected. The daemon believed this
		// unit was awake and skipped asking the workload; the dial says otherwise, so the
		// belief was wrong - something outside sbx stopped or killed the container.
		//
		// Revoke it and wake properly, once. Without this the optimism would be permanent:
		// a container removed by hand, or a docker daemon restart, would leave every future
		// connection failing against a unit sbx still thought was up.
		u.setAwake(false)

		if err := u.wake(ctx, p, readyTimeout); err != nil {
			logs.Default.Event(logs.LevelError, u.sandbox, u.service, "wakeFailed", 0,
				"could not wake after an unreachable upstream: %v", err)
			return
		}

		upstream, err = l.dial()
		if err != nil {
			logs.Default.Error(u.sandbox, u.service, "awake but not reachable at %s: %v", l.Upstream, err)
			return
		}
	}
	defer upstream.Close()

	u.track(client)
	defer u.untrack(client)

	// Both directions, not the first to finish.
	//
	// Waiting on one made the CloseWrite in pipe() dead: the moment either side saw EOF,
	// handle() returned and the deferred Closes killed the other direction mid-flight. A
	// client that says "that is all I am sending" by shutting down its write side - `nc -N`,
	// several HTTP clients, any pipe-mode bulk loader - then had its response truncated
	// rather than completed. Silently, which is the worst shape for a proxy whose job is to
	// be invisible.
	//
	// This cannot hang on a peer that stays open: the half-close gives it EOF, and a
	// connection nobody ever closes is torn down when the sandbox sleeps, which closes every
	// tracked client.
	done := make(chan struct{}, 2)
	go u.pipe(upstream, client, done)
	go u.pipe(client, upstream, done)
	<-done
	<-done
}

// relayBuf is the copy buffer for one direction of a tunnel.
//
// 64 KiB, up from 32, and pooled. The size is the throughput lever: a bulk transfer's cost is
// dominated by the read/write syscall pair per chunk, so a bigger buffer drains a loopback socket
// in fewer syscalls. 64 is the measured sweet spot, not a guess: in a reserved-core benchmark it
// ran +36% over 32 KiB and with the tightest variance, while 128 and 256 KiB were both slower and
// far noisier - a buffer that no longer fits the cache thrashes it. See docs/BENCHMARKS.md. The
// pool is the connection-churn lever: the old code allocated two fresh buffers on every
// connection, which a client opening hundreds of short ones - an agent hammering redis - turned
// into steady GC pressure; from the pool it is zero allocation after warmup.
var relayBuf = 64 << 10

var relayBufPool = sync.Pool{New: func() any { b := make([]byte, relayBuf); return &b }}

// pipe copies one direction, stamping activity as it goes.
func (u *unit) pipe(dst, src net.Conn, done chan<- struct{}) {
	bp := relayBufPool.Get().(*[]byte)
	defer relayBufPool.Put(bp)

	buf := *bp

	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			u.touch()

			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}

		if rerr != nil {
			break
		}
	}

	// Half-close so the peer sees EOF instead of waiting on a connection nobody will write
	// to again.
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}

	done <- struct{}{}
}

// wake starts the workload if needed and blocks until it reports serving. The caller's
// first query pays this; nothing else ever does.
// wake brings this unit up, and whatever it declared it needs, before returning.
//
// Split from wakeSelf so that waking a dependency cannot recurse back through the dependency
// walk: wakeAll calls wakeSelf directly, having already handled that unit's own chain.
func (u *unit) wake(ctx context.Context, p provider.Provider, readyTimeout time.Duration) error {
	if err := u.wakeDeps(ctx, p, readyTimeout); err != nil {
		return err
	}

	return u.wakeSelf(ctx, p, readyTimeout)
}

func (u *unit) wakeSelf(ctx context.Context, p provider.Provider, readyTimeout time.Duration) error {
	// A unit the daemon woke and has not slept is awake, and the daemon is the only thing
	// that sleeps one. Asking the workload again costs a `docker exec` - measured at 68 ms
	// median per connection against 0.8 ms straight to docker - and it was being paid on
	// every new connection for the life of the sandbox, not just the first.
	//
	// That made the docstring above false and the published proxy overhead (~33 µs) true
	// only of bytes on an already-open connection. Anything that opens a connection per
	// operation - psql, a CLI, a client with no pool - paid the exec every time.
	//
	// The lock is still taken on every connection - only the probe is skipped. That
	// distinction is the whole fix: the 68 ms was `docker exec`, not the mutex, which is
	// uncontended and costs nanoseconds.
	//
	// Taking it matters because sleep() holds the same lock. Checking awake outside it let a
	// connection arrive, see "awake", and dial a container that sleep() was already
	// committed to stopping - the client got a connection reset for a sandbox it had just
	// woken. Rare, and reached twice in one run of the concurrent-wake suite on a busy
	// machine. Serialising against sleep() is what makes "awake" mean anything.
	u.waking.Lock()
	defer u.waking.Unlock()

	// Awake, and now known to be so for as long as this lock is held: nothing can sleep it
	// between here and the caller's dial. A burst of connections to one sleeping unit blocks
	// here too, so only the first of them starts the container.
	//
	// Optimistic, and corrected rather than trusted: if the container died underneath us the
	// dial in handle() fails, and that is where this belief gets revoked and retried.
	if u.isAwake() {
		// Being needed counts, even when nothing had to be started.
		//
		// This is the common case for a dependency in a running stack, and stamping only the
		// cold path below left it uncovered: the dependency is already up, so this returns
		// here, and the reaper's `needed` set is built from units that are ALREADY awake - so
		// while the dependent is still starting it is in neither. A db last used 4m58s ago,
		// with a 5m window and an app that takes twenty seconds to report serving, is slept
		// underneath the wake that asked for it, and the app comes up to `no such host`.
		//
		// It cannot hold anything awake that should sleep: the only callers are handle(),
		// which has already touched for the byte it is about to relay, and a dependency walk,
		// which is somebody genuinely needing this unit right now.
		u.touch()

		return nil
	}

	start := time.Now()

	// Straight to Start, with no probe first.
	//
	// There used to be one, to catch a container someone had started outside sbx. It cost a
	// round trip on every cold wake to answer a question Start answers for free: starting an
	// already-running container is a 304 the provider treats as success. So on the path it
	// was actually on - the unit is asleep, which is why we are here - it could only fail,
	// and the poll loop below reaches the same conclusion one iteration later.
	if err := p.Start(ctx, u.ref); err != nil {
		return err
	}

	deadline := time.Now().Add(readyTimeout)

	// A flat interval, after a backoff was tried and refuted.
	//
	// The argument for backing off from 5 ms is good on paper: a fixed 100 ms sleep rounds a
	// fast workload up to the interval, and a counting test agreed - an 8 ms workload was
	// declared awake in 102 ms with a flat poll and about 20 ms with a backoff.
	//
	// It is not what happens, because that test assumed probes are free and they are not.
	// Each one is an Engine API exec, so probing at 5 ms does not sample sooner than the
	// probe itself costs, and a real workload is not ready in 8 ms anyway. Measured
	// end-to-end, interleaved with the order alternating, n=14: 162 ms flat against 166 ms
	// backing off - no difference, with more traffic to produce it.
	wait := 100 * time.Millisecond

	for time.Now().Before(deadline) {
		serving, declared := p.Probe(ctx, u.ref)

		// Without a declared health check there is nothing to ask, and dialling the port
		// is not an answer: a published port answers before the server behind it does.
		// Give the workload a moment and go, rather than pretend to have checked.
		if !declared {
			time.Sleep(2 * time.Second)
			u.setAwake(true)
			u.touch()
			logs.Default.Event(logs.LevelWarn, u.sandbox, u.service, "woke",
				time.Since(start).Milliseconds(),
				"woke in %dms, unverified - no health check declared",
				time.Since(start).Milliseconds())

			return nil
		}

		if serving {
			u.setAwake(true)

			// Coming up counts as activity, or a unit woken ONLY as somebody else's dependency
			// starts life already past the idle window: nothing dials a dependency through the
			// proxy, so no byte ever reaches its leg. The reaper could then sleep it inside the
			// window where its dependent is still starting - before that dependent is awake and
			// protecting it - and the dependent would come up into a sandbox whose datastore had
			// just been stopped. `no such host`, from the one direction still open after the
			// reaper learned to respect depends_on.
			u.touch()

			u.mu.Lock()
			u.served = true
			u.mu.Unlock()

			logs.Default.Event(logs.LevelInfo, u.sandbox, u.service, "woke",
				time.Since(start).Milliseconds(), "woke in %dms", time.Since(start).Milliseconds())

			return nil
		}

		time.Sleep(wait)
	}

	return errNotReady{u.name, readyTimeout}
}

// sleep stops the workload and closes anything still attached to it. Closing the live
// connections is not rudeness: the client reconnects, the reconnect wakes the unit, and the
// alternative is a pool holding a sandbox up for a branch nobody is working on.
// sleep stops the unit. It takes the same lock wake() does, because the two drive one
// container in opposite directions and the interleaving is visible to a client.
//
// Without the lock a connection arriving mid-stop found Probe still reporting the old
// state, was told the sandbox was awake, and was handed a connection to a container that
// was in the process of stopping. The daemon then recorded it asleep, so the daemon and
// the caller disagreed about the same sandbox.
//
// idle is re-checked here rather than trusted from the reaper: between the reaper deciding
// and this acquiring the lock a client may have arrived and touched the unit, and stopping
// then is stopping something in use.
func (u *unit) sleep(ctx context.Context, p provider.Provider, idle time.Duration) {
	u.waking.Lock()
	defer u.waking.Unlock()

	if u.idleFor() < idle {
		return
	}

	u.mu.Lock()
	for c := range u.live {
		_ = c.Close()
	}

	u.live = map[net.Conn]struct{}{}
	u.mu.Unlock()

	// Marked asleep before the stop returns, so anything blocked on the lock behind this
	// takes the wake path instead of believing a stale "awake".
	u.setAwake(false)

	if err := p.Stop(ctx, u.ref); err != nil {
		logs.Default.Error(u.sandbox, u.service, "could not sleep: %v", err)
		return
	}
	logs.Default.Event(logs.LevelInfo, u.sandbox, u.service, "slept",
		u.idleFor().Milliseconds(), "slept - idle for %s", u.idleFor().Round(time.Second))
}

func (u *unit) track(c net.Conn) {
	u.mu.Lock()
	u.live[c] = struct{}{}
	u.mu.Unlock()
}

func (u *unit) untrack(c net.Conn) {
	u.mu.Lock()
	delete(u.live, c)
	u.mu.Unlock()
	u.touch()
}

func (u *unit) isAwake() bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.awake
}

func (u *unit) setAwake(v bool) {
	u.mu.Lock()
	u.awake = v
	u.mu.Unlock()
}

// listenAddr binds loopback on a laptop and every interface in a pod, because there the
// connection arrives from a Service on the pod network rather than from this machine.
func listenAddr(port int) string {
	if InCluster() {
		return "0.0.0.0:" + itoa(port)
	}

	return "127.0.0.1:" + itoa(port)
}

// dial opens the connection to the workload behind this leg.
//
// Two paths, and the difference is whether the address had to be looked up. A resolved literal
// is dialled directly - no string to format, no string to parse, no resolver, no deadline
// context and timer - which is 17 of the 38 allocations a connection costs.
//
// The timeout goes with them, and that is the trade. It is safe on a literal because a literal
// is what the docker provider hands out - 127.0.0.1:<backing port> - and connect() to loopback
// either succeeds or returns ECONNREFUSED at once; there is nothing for a timeout to bound. It
// is NOT safe on a name: a cluster Service with no endpoints blackholes, and without a bound
// handle() would sit on the OS default of a minute or more holding the client and two
// goroutines, never reaching the revoke-and-rewake below. So a name keeps DialTimeout.
//
// Either way a refused dial returns promptly, which is what that recovery path needs.
func (l *leg) dial() (net.Conn, error) {
	if l.addr != nil {
		return net.DialTCP("tcp", nil, l.addr)
	}

	return net.DialTimeout("tcp", l.Upstream.String(), 10*time.Second)
}
