package daemon

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/osb"
)

// openSandboxAPI builds the OpenSandbox lifecycle API and binds its listener, or returns nil
// when --osb-addr was not given. Bound here, before the daemon starts, so a port that is taken
// is a startup error with the address in it rather than a log line after everything else is up.
func (d *daemon) openSandboxAPI(addr, key string, hostPaths []string, scope Scope, pools []string, poolFreeze bool) (*osb.Server, net.Listener, error) {
	if addr == "" {
		if len(pools) > 0 {
			return nil, nil, fmt.Errorf("--osb-pool keeps sandboxes warm for the OpenSandbox API, which is off - add --osb-addr 127.0.0.1:8080")
		}

		return nil, nil, nil
	}

	var specs []osb.PoolSpec

	for _, v := range pools {
		sp, err := osb.ParsePool(v)
		if err != nil {
			return nil, nil, err
		}

		specs = append(specs, sp)
	}

	if key == "" {
		key = os.Getenv("SBX_OSB_KEY")
	}

	if d.provider == nil {
		return nil, nil, fmt.Errorf("--osb-addr needs a container runtime to create sandboxes in, "+
			"and this daemon has none: %v", d.startupErr)
	}

	if err := osb.CheckBind(addr, key); err != nil {
		return nil, nil, err
	}

	// Every id the API mints is osb-<12 hex>. A scope that excludes them would create sandboxes
	// this daemon then refuses to front - endpoints that never answer.
	if !scope.Match("osb-000000000000") {
		return nil, nil, fmt.Errorf("--osb-addr creates sandboxes named osb-..., which --only %s "+
			"excludes; add --only osb-", scope)
	}

	// Bound before the key, like every other refusal that does not depend on it: a port already
	// taken is a refusal too, and says nothing about keys.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("--osb-addr %s: %w", addr, err)
	}

	// The key last, after every refusal that does not depend on it. It used to come first, so a
	// start refused for --osb-addr 0.0.0.0:8080 had already minted ~/.sbx/osb/key - a credential
	// on disk for an API that never ran, which `sbx mcp` would then find and use.
	key, err = d.osbKey(addr, key)
	if err != nil {
		_ = ln.Close()
		return nil, nil, err
	}

	api, err := osb.New(osb.Options{
		Provider:     d.provider,
		Runtime:      d,
		Key:          key,
		Owner:        "sbx-serve:" + scope.String(),
		Version:      logs.Version,
		ReadyTimeout: d.ready + 30*time.Second,
		Egress:       d.Egress(),
		EgressStatus: EgressHTTPStatus,
		HostPaths:    hostPaths,
		Pools:        specs,
		PoolFreeze:   poolFreeze,
	})
	if err != nil {
		_ = ln.Close()
		return nil, nil, err
	}

	d.servesOSB = true

	return api, ln, nil
}

// The daemon's EgressControl is what the API's networkpolicy routes drive.
var _ osb.EgressAPI = (*EgressControl)(nil)

// splitPaths reads --osb-host-paths: comma-separated, blanks dropped.
func splitPaths(s string) []string {
	var out []string

	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out
}
