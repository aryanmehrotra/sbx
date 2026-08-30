package daemon

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Where the filter runs as a container, the daemon cannot watch its traffic the way it watches
// its own listener - the container is on the sandbox's bridge, inside the VM, and the daemon is
// out here. So it asks. The filter publishes the timestamp of its last permitted byte on a
// loopback port, and this reads it once a refresh tick and stamps the sandbox when it has moved.
//
// A tick is seconds and an idle window is minutes, so the lag is an order of magnitude inside
// the decision it feeds. It is a poll rather than a callback because the alternative is the
// container holding a connection back to the daemon, which is a second thing to keep alive and
// to reconnect, to learn something that is only consulted once a tick anyway.

// scrapeTimeout is deliberately short. This runs on the refresh tick, and a filter that is slow
// to answer must not hold up everything else the tick does; a missed reading costs at most one
// tick of staleness on a window measured in minutes.
const scrapeTimeout = 2 * time.Second

// scrapeEgress reads every container filter's activity endpoint and stamps the units behind it
// when the reading has advanced since last time.
func (d *daemon) scrapeEgress(ctx context.Context, found []provider.Unit) {
	stats := map[string]string{} // gateway -> loopback stat address

	for _, u := range found {
		if u.EgressStat != "" && u.EgressGateway != "" {
			stats[u.EgressGateway] = u.EgressStat
		}
	}

	for gw, addr := range stats {
		last, err := readLastActivity(ctx, addr)
		if err != nil {
			logs.Default.Debug("", "", "egress filter %s did not answer: %v", addr, err)
			continue
		}

		d.mu.Lock()
		seen := d.egressSeen[gw]
		if last > seen {
			d.egressSeen[gw] = last
		}
		d.mu.Unlock()

		// Only when it moved. Otherwise every tick would stamp the sandbox awake and nothing
		// with a filter would ever sleep - the exact bug this feature is meant to avoid the
		// other way round.
		if last > seen {
			d.touchEgress(gw)
		}
	}
}

// readLastActivity asks one filter when it last carried a permitted byte.
func readLastActivity(ctx context.Context, addr string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/last", nil)
	if err != nil {
		return 0, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return 0, err
	}

	return strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
}
