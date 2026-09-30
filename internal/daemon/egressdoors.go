package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A container filter is started with the doors the engine had then: every docker network's
// gateway and the default bridge's subnet (egress.Doors). Another sandbox created afterwards brings
// a network whose gateway is also the VM, and the filter never heard of it - `CONNECT 172.31.0.1:443`
// was dialled, and answered by the VM. The filter has no docker socket and must never get one, so
// the daemon, which has, lists the doors once a discovery tick and pushes them to every container
// filter over the token-guarded control endpoint. The filter unions them with its own.
//
// Pushed every tick rather than on change: the filter saves the last push, but a replaced filter
// starts from its create-time list, and the only way to know which is which from here is to ask.
// Asking costs the same loopback request as telling. The listing is two docker calls a tick, made
// only while a container filter exists.

// filterEndpoint is where a sandbox's container filter answers, and the token it wants.
type filterEndpoint struct {
	addr, token string
}

// filterEndpoints caches filterEndpoint per sandbox. The token and the published port change only
// when the container is replaced or restarted, and then the next request fails and the entry is
// dropped, so the steady state costs no docker call.
type filterEndpoints struct {
	mu sync.Mutex
	m  map[string]filterEndpoint
}

// filterEndpoint returns the sandbox's container filter endpoint, or false when it has none (a
// filter the daemon hosts) or it cannot be found right now.
func (d *daemon) filterEndpoint(ctx context.Context, sandbox string) (filterEndpoint, bool) {
	d.endpoints.mu.Lock()
	ep, ok := d.endpoints.m[sandbox]
	d.endpoints.mu.Unlock()

	if ok {
		return ep, true
	}

	f, err := d.egressControl().locate(ctx, sandbox, "", false)
	if err != nil || f.Control == "" || f.Token == "" {
		if err != nil {
			logs.Default.Debug(sandbox, "", "egress filter endpoint: %v", err)
		}

		return filterEndpoint{}, false
	}

	ep = filterEndpoint{addr: f.Control, token: f.Token}

	d.endpoints.mu.Lock()
	if d.endpoints.m == nil {
		d.endpoints.m = map[string]filterEndpoint{}
	}
	d.endpoints.m[sandbox] = ep
	d.endpoints.mu.Unlock()

	return ep, true
}

// forgetFilterEndpoint drops a cached endpoint that did not work, so the next tick looks again.
func (d *daemon) forgetFilterEndpoint(sandbox string) {
	d.endpoints.mu.Lock()
	delete(d.endpoints.m, sandbox)
	d.endpoints.mu.Unlock()
}

// pushEgressDoors tells every container filter in found which addresses are the machine behind
// it, as docker has them now.
func (d *daemon) pushEgressDoors(ctx context.Context, found []provider.Unit) {
	var sandboxes []string

	seen := map[string]bool{}

	for _, u := range found {
		if u.EgressStat != "" && !seen[u.Sandbox] {
			seen[u.Sandbox] = true
			sandboxes = append(sandboxes, u.Sandbox)
		}
	}

	if len(sandboxes) == 0 {
		return
	}

	ds, ok := d.provider.(provider.EgressDoorSets)
	if !ok {
		return
	}

	doors, err := ds.EgressDoors(ctx)
	if err != nil {
		// Nothing is pushed, so every filter keeps the last set it was given. Said at info: until
		// this clears, a network created from now on is not refused by filters already running.
		logs.Default.Info("", "", "egress doors not refreshed: %v", err)
		return
	}

	body, err := json.Marshal(egress.RefuseSet{Prefixes: doors})
	if err != nil {
		return
	}

	for _, sb := range sandboxes {
		ep, ok := d.filterEndpoint(ctx, sb)
		if !ok {
			continue
		}

		if err := putRefuse(ctx, ep, body); err != nil {
			d.forgetFilterEndpoint(sb)
			logs.Default.Debug(sb, "", "egress doors push: %v", err)
		}
	}
}

// putRefuse sends one filter its refuse set.
func putRefuse(ctx context.Context, ep filterEndpoint, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+ep.addr+"/refuse", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set(egress.TokenHeader, ep.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// A filter from an older sbx answers 404: it has no /refuse, and keeps its start list
		// until the sandbox is recreated (ensureFilterContainer replaces a filter built by an
		// older sbx on the next create).
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}

	return nil
}
