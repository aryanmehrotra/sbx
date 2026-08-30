package daemon

import (
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// egressProxy is one running EgressFilter: the listener on a sandbox's bridge gateway and the
// allow-list it was started with, so a changed list can be spotted and the proxy restarted.
type egressProxy struct {
	allow string
	ln    net.Listener

	// lastTouch is when this proxy last stamped its sandbox awake, as UnixNano.
	//
	// Stamping walks the unit map under the daemon lock, and a streaming response calls it
	// once per 32 KiB chunk - so it is throttled to once a second per gateway. Idle windows
	// are minutes, which makes a second finer granularity than the decision ever needs, and
	// it keeps a busy download off the lock the wake path also takes.
	lastTouch atomic.Int64
}

// due reports whether enough time has passed to walk the unit map again, and claims the slot if
// so. The CAS is what makes two concurrent streams cost one walk rather than two.

// reconcileEgress keeps exactly one filtering proxy running per sandbox that declared an egress
// allow-list, bound to that sandbox's no-NAT bridge gateway - the one address a container on the
// bridge can reach, and the only way out, since the bridge denies the direct route. Started when
// the sandbox appears, stopped when it is gone, restarted if the list changed. Services that
// share a sandbox share its bridge and so its proxy, with the union of their allow-lists.
//
// It is off the wake path: a sandbox with no allow-list gets none of this, and the proxy runs on
// its own listener, never touching the byte-splice the wake numbers are measured on.
func (d *daemon) reconcileEgress(found []provider.Unit) {
	lists := map[string][]string{} // gateway -> union of allow-lists

	for _, u := range found {
		if len(u.EgressAllow) == 0 || u.EgressGateway == "" {
			continue
		}

		for _, h := range u.EgressAllow {
			if !slices.Contains(lists[u.EgressGateway], h) {
				lists[u.EgressGateway] = append(lists[u.EgressGateway], h)
			}
		}
	}

	desired := map[string]string{}

	for gw, l := range lists {
		sort.Strings(l)
		desired[gw] = strings.Join(l, ",")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Stop proxies whose sandbox is gone, or whose allow-list changed (it will be recreated
	// below with the new one).
	for gw, p := range d.egress {
		if desired[gw] != p.allow {
			_ = p.ln.Close()
			delete(d.egress, gw)
		}
	}

	// Start the ones that should be running and are not.
	for gw, allow := range desired {
		if _, ok := d.egress[gw]; ok {
			continue
		}

		addr := net.JoinHostPort(gw, strconv.Itoa(provider.EgressProxyPort))

		ln, err := net.Listen("tcp", addr)
		if err != nil {
			logs.Default.Warn("", "", "egress filter could not bind %s: %v", addr, err)
			continue
		}

		filter := NewEgressFilter(lists[gw])
		filter.OnActivity = func() { d.touchEgress(gw) }
		srv := &http.Server{Handler: filter}
		go func() { _ = srv.Serve(ln) }()

		d.egress[gw] = &egressProxy{allow: allow, ln: ln}
		logs.Default.Info("", "", "egress allow-list active on %s: %s", addr, allow)
	}
}
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

	p, ok := d.egress[gw]
	if !ok || !p.due(time.Now().UnixNano()) {
		return
	}

	for _, u := range d.units {
		if u.egressGateway == gw {
			u.touch()
		}
	}
}
