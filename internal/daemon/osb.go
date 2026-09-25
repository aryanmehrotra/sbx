package daemon

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/osb"
)

// openSandboxAPI builds the OpenSandbox lifecycle API and binds its listener, or returns nil
// when --osb-addr was not given. Bound here, before the daemon starts, so a port that is taken
// is a startup error with the address in it rather than a log line after everything else is up.
func (d *daemon) openSandboxAPI(addr, key string, scope Scope) (*osb.Server, net.Listener, error) {
	if addr == "" {
		return nil, nil, nil
	}

	if key == "" {
		key = os.Getenv("SBX_OSB_KEY")
	}

	key, err := d.osbKey(addr, key)
	if err != nil {
		return nil, nil, err
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

	api, err := osb.New(osb.Options{
		Provider:     d.provider,
		Runtime:      d,
		Key:          key,
		Owner:        "sbx-serve:" + scope.String(),
		Version:      logs.Version,
		ReadyTimeout: d.ready + 30*time.Second,
		Egress:       d.Egress(),
		EgressStatus: EgressHTTPStatus,
	})
	if err != nil {
		return nil, nil, err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("--osb-addr %s: %w", addr, err)
	}

	d.servesOSB = true

	return api, ln, nil
}

// The daemon's EgressControl is what the API's networkpolicy routes drive.
var _ osb.EgressAPI = (*EgressControl)(nil)
