package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// egressProxy is one running egress.Filter: the listener on a sandbox's bridge gateway, the
// filter behind it, and what it was started for - so a live change can be applied to it in
// place, and a sandbox that went away can be told apart from one whose policy changed.
type egressProxy struct {
	sandbox string
	ln      net.Listener
	filter  *egress.Filter

	// declared is the policy the sandbox's spec gives it, and savedAt the modification time of
	// the live copy last read from disk - how watchEgress notices a change the CLI made.
	declared egress.Policy
	savedAt  time.Time

	// lastTouch is when this proxy last stamped its sandbox awake, as UnixNano.
	//
	// Stamping walks the unit map under the daemon lock, and a streaming response calls it
	// once per 32 KiB chunk - so it is throttled to once a second per gateway. Idle windows
	// are minutes, which makes a second finer granularity than the decision ever needs, and
	// it keeps a busy download off the lock the wake path also takes.
	lastTouch atomic.Int64
}

// Egress is the daemon's egress policy API. Writes through it reach the filters this daemon
// hosts at once, and container filters over their control endpoint.
func (d *daemon) Egress() *EgressControl { return d.egressControl() }

func (d *daemon) egressControl() *EgressControl {
	d.egressOnce.Do(func() {
		d.egressCtl = NewEgressControl(d.provider, d.egressDir)
		d.egressCtl.apply = d.applyEgress
	})

	return d.egressCtl
}

// applyEgress swaps the policy of the filter this daemon hosts on gw, if it hosts one.
func (d *daemon) applyEgress(gw string, p egress.Policy) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	proxy, ok := d.egress[gw]
	if !ok {
		return false
	}

	if err := proxy.filter.SetPolicy(p); err != nil {
		logs.Default.Warn(proxy.sandbox, "", "egress policy refused: %v", err)
		return false
	}

	logs.Default.Info(proxy.sandbox, "", "egress policy changed live: %s (%s)", p.Mode(), p.Hash())

	return true
}

// effectivePolicy is what a hosted filter for sandbox should enforce: the live policy saved on
// this host if there is one made against the same declaration, else the declaration.
func (d *daemon) effectivePolicy(sandbox string, declared egress.Policy) (egress.Policy, time.Time) {
	c := d.egressControl()

	path, err := c.path(sandbox)
	if err != nil {
		return declared, time.Time{}
	}

	st, err := os.Stat(path)
	if err != nil {
		return declared, time.Time{}
	}

	if s, ok := c.load(sandbox); ok && s.Declared == declared.Hash() {
		return s.Policy, st.ModTime()
	}

	return declared, st.ModTime()
}

// reconcileEgress keeps exactly one filtering proxy running per sandbox that is filtered and
// whose filter this machine can host, bound to that sandbox's no-NAT bridge gateway - the one
// address a container on the bridge can reach, and the only way out, since the bridge denies the
// direct route. Started when the sandbox appears, stopped when it is gone. A changed policy is
// applied to the running filter in place, never by restarting it: a restart would drop every
// tunnel open through it, and "change the policy without recreating anything" is the point.
//
// It is off the wake path: a sandbox with no filter gets none of this, and the proxy runs on its
// own listener, never touching the byte-splice the wake numbers are measured on.
func (d *daemon) reconcileEgress(found []provider.Unit) {
	type want struct {
		sandbox string
		units   []provider.Unit
	}

	wants := map[string]*want{} // gateway -> the filtered units behind it

	for _, u := range found {
		if u.EgressGateway == "" {
			continue
		}

		// A container already holds this gateway's filter, because this machine could not.
		// Binding it here would fail every tick and log a warning for a sandbox that is in
		// fact correctly filtered - and on the one machine where the bind DID succeed, it
		// would put a second filter in front of the first.
		if u.EgressStat != "" {
			continue
		}

		w := wants[u.EgressGateway]
		if w == nil {
			w = &want{sandbox: u.Sandbox}
			wants[u.EgressGateway] = w
		}

		w.units = append(w.units, u)
	}

	type desired struct {
		sandbox  string
		declared egress.Policy
		policy   egress.Policy
		savedAt  time.Time
	}

	next := map[string]desired{}

	for gw, w := range wants {
		declared := provider.DeclaredPolicy(w.units)
		p, at := d.effectivePolicy(w.sandbox, declared)
		next[gw] = desired{sandbox: w.sandbox, declared: declared, policy: p, savedAt: at}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for gw, p := range d.egress {
		want, ok := next[gw]

		// Gone, or the gateway now belongs to a different sandbox: that is a new filter, not a
		// policy change, and the old one must not keep serving under the new name.
		if !ok || want.sandbox != p.sandbox {
			_ = p.ln.Close()
			delete(d.egress, gw)

			continue
		}

		p.declared, p.savedAt = want.declared, want.savedAt

		if p.filter.Policy().Hash() != want.policy.Hash() {
			if err := p.filter.SetPolicy(want.policy); err == nil {
				logs.Default.Info(p.sandbox, "", "egress policy now %s (%s)", want.policy.Mode(), want.policy.Hash())
			}
		}
	}

	// Start the ones that should be running and are not.
	for gw, want := range next {
		if _, ok := d.egress[gw]; ok {
			continue
		}

		addr := net.JoinHostPort(gw, strconv.Itoa(provider.EgressProxyPort))

		ln, err := net.Listen("tcp", addr)
		if err != nil {
			logs.Default.Warn("", "", "egress filter could not bind %s: %v", addr, err)
			continue
		}

		filter := egress.NewPolicy(want.policy)
		filter.OnActivity = func() { d.touchEgress(gw) }
		srv := &http.Server{Handler: filter}
		go func() { _ = srv.Serve(ln) }()

		d.egress[gw] = &egressProxy{sandbox: want.sandbox, ln: ln, filter: filter,
			declared: want.declared, savedAt: want.savedAt}
		logs.Default.Info(want.sandbox, "", "egress filter on %s: %s (%s)", addr, want.policy.Mode(), want.policy.Hash())
	}
}

// watchEgress applies a live policy written by another process - `sbx egress` - to the filters
// this daemon hosts, within a second rather than on the next discovery tick. One stat per hosted
// filter per second: nothing, next to what the filter itself costs.
func (d *daemon) watchEgress(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		d.mu.Lock()
		type check struct {
			sandbox  string
			declared egress.Policy
			savedAt  time.Time
		}

		var checks []check

		for _, p := range d.egress {
			checks = append(checks, check{p.sandbox, p.declared, p.savedAt})
		}
		d.mu.Unlock()

		for _, c := range checks {
			if _, at := d.effectivePolicy(c.sandbox, c.declared); !at.Equal(c.savedAt) {
				d.refreshHosted(c.sandbox)
			}
		}
	}
}

// refreshHosted re-reads a hosted sandbox's policy and applies it if it changed.
func (d *daemon) refreshHosted(sandbox string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, p := range d.egress {
		if p.sandbox != sandbox {
			continue
		}

		want, at := d.effectivePolicy(sandbox, p.declared)
		p.savedAt = at

		if p.filter.Policy().Hash() != want.Hash() && p.filter.SetPolicy(want) == nil {
			logs.Default.Info(sandbox, "", "egress policy changed live: %s (%s)", want.Mode(), want.Hash())
		}
	}
}

// syncEgress pushes saved live policies to container filters that have drifted from them. Only
// sandboxes with a saved policy are asked, so a machine where nobody has changed a policy live
// pays nothing for this.
func (d *daemon) syncEgress(ctx context.Context, found []provider.Unit) {
	seen := map[string]bool{}

	for _, u := range found {
		if u.EgressStat == "" || seen[u.Sandbox] {
			continue
		}

		seen[u.Sandbox] = true

		if _, ok := d.egressControl().load(u.Sandbox); !ok {
			continue
		}

		if err := d.egressControl().Sync(ctx, u.Sandbox); err != nil {
			logs.Default.Debug(u.Sandbox, "", "egress policy sync: %v", err)
		}
	}
}

// due reports whether enough time has passed to walk the unit map again, and claims the slot if
// so. The CAS is what makes two concurrent streams cost one walk rather than two.
func (p *egressProxy) due(now int64) bool {
	last := p.lastTouch.Load()
	if now-last < int64(time.Second) {
		return false
	}

	return p.lastTouch.CompareAndSwap(last, now)
}

// touchEgress stamps every unit behind a gateway as active, because something inside the
// sandbox just talked to the outside world through the filter.
//
// This is the idle signal for the box that nothing dials. A sandbox running an agent takes no
// inbound connection - it reads files, compiles, and calls an API - so on the inbound bytes sbx
// measures it looks idle from the moment it starts working, and the only setting that kept it
// alive was idle: "never", which holds its memory for as long as the sandbox exists. An
// allow-listed box's API calls come through code sbx already owns, so they can be counted.
//
// Every unit on the gateway is stamped, not just one: the gateway is the sandbox's bridge, the
// allow-list is the union of its services', and there is nothing in an HTTP CONNECT that says
// which container opened it.
func (d *daemon) touchEgress(gw string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// The throttle belongs to the in-process filter, which calls this once per copied chunk.
	// A gateway with no proxy here is one whose filter is a CONTAINER: its activity arrives by
	// scrape, already at most once a refresh tick, and looking for a throttle that was never
	// created would drop the stamp entirely. That is not hypothetical - it is what this did,
	// and a box calling out every five seconds slept anyway, because the lookup failed before
	// the walk was ever reached.
	if p, ok := d.egress[gw]; ok && !p.due(time.Now().UnixNano()) {
		return
	}

	d.stampGateway(gw)
}

// stampGateway marks every unit behind a gateway as active. Callers hold d.mu.
func (d *daemon) stampGateway(gw string) {
	for _, u := range d.units {
		if u.egressGateway == gw {
			u.touch()
		}
	}
}
