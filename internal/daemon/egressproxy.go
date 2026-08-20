package daemon

import (
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// egressProxy is one running EgressFilter: the listener on a sandbox's bridge gateway and the
// allow-list it was started with, so a changed list can be spotted and the proxy restarted.
type egressProxy struct {
	allow string
	ln    net.Listener
}

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

		srv := &http.Server{Handler: NewEgressFilter(lists[gw])}
		go func() { _ = srv.Serve(ln) }()

		d.egress[gw] = &egressProxy{allow: allow, ln: ln}
		logs.Default.Info("", "", "egress allow-list active on %s: %s", addr, allow)
	}
}
