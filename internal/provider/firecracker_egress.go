package provider

import (
	"context"
	"encoding/json"
	"net"
	"strconv"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// Egress for a microVM: the same filter, the same door, the same rule as docker's no-NAT bridge.
//
// A VM's bridge (sbxfc<slot>) has no NAT and sbx writes no route off it, so a guest has no way
// out of its own - which is what `egress: "deny"` always was here. A filtered service gets one
// door: the daemon's egress filter, listening on the bridge's gateway address, which is the one
// host address a guest can reach. HTTP(S)_PROXY points ordinary clients at it; a client that
// ignores it and dials out directly has no route, so the policy is enforced rather than advised.
// `egress: "allow"` is the same door with an open default, so it carries HTTP and HTTPS and
// nothing else - DECISIONS.md, "Default-allow is enforced by the same door, and it costs raw TCP".

// declaredJSON is the policy a filtered service declares, as the record keeps it.
func declaredJSON(svc spec.Service) (string, error) {
	b, err := json.Marshal(svc.DeclaredPolicy())
	return string(b), err
}

// egressProxyURL is the filter as a guest on a's bridge reaches it.
func egressProxyURL(a fc.Addr) string {
	return "http://" + net.JoinHostPort(a.Gateway(), strconv.Itoa(EgressProxyPort))
}

// withEgressProxy points the guest's clients at its filter. It replaces a proxy the image or the
// spec set, as docker's `-e` after the spec's does: a proxy elsewhere is a route this bridge
// does not have, so keeping it would only make every request fail differently.
func withEgressProxy(env []string, a fc.Addr) []string {
	proxy := egressProxyURL(a)
	keys := []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"}
	set := make(map[string]string, len(keys))

	for _, k := range keys {
		set[k] = proxy
	}

	return fc.MergeEnv(env, set, keys)
}

func filteredWord(filtered bool) string {
	if filtered {
		return "with an egress filter"
	}

	return "without an egress filter"
}

// EgressFilter finds a microVM sandbox's filter. It is always the daemon's own listener on the
// bridge gateway - there is no container to hold it, since the daemon runs where the bridge is -
// so a change reaches it through the daemon (EgressControl.apply) or, from the CLI, through the
// saved copy the daemon watches.
func (p *fcProvider) EgressFilter(ctx context.Context, sandbox string) (EgressFilter, error) {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return EgressFilter{}, err
	}

	return filterOf(sandbox, units)
}
