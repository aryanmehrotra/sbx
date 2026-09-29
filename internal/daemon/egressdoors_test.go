package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// doorsProvider is a docker engine as the daemon sees it: one sandbox's container filter, and a
// network list that grows when another sandbox is created.
type doorsProvider struct {
	filterProvider

	mu    sync.Mutex
	doors []string
	err   error
}

func (p *doorsProvider) EgressDoors(context.Context) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.doors...), p.err
}

// containerDoors is a container filter's refused set as main builds it, minus the machine it runs
// on: no routes, no names.
func containerDoors(t *testing.T, static string) *egress.Doors {
	t.Helper()

	s, err := egress.ParsePrefixes(static)
	if err != nil {
		t.Fatal(err)
	}

	d := &egress.Doors{
		Static:  s,
		Routes:  func() (string, error) { return "", nil },
		Resolve: func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("none") },
	}
	d.Refresh(context.Background())

	return d
}

func newDoorsSetup(t *testing.T, token string) (*daemon, *doorsProvider, *egress.Doors, []provider.Unit) {
	t.Helper()

	doors := containerDoors(t, "172.30.0.1,172.17.0.0/16")
	srv := httptest.NewServer(&egress.Control{
		Filter: egress.NewPolicy(declared()), Doors: doors, Token: "tok",
		Last: func() int64 { return 42 },
	})
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "http://")
	fp := &doorsProvider{
		filterProvider: filterProvider{f: provider.EgressFilter{
			Sandbox: "osb-1", Gateway: "172.30.0.1", Services: []string{"sandbox"},
			Declared: declared(), Control: addr, Token: token,
		}},
		doors: []string{"172.30.0.1", "172.17.0.1", "172.17.0.0/16"},
	}

	d := New(fp, time.Minute, time.Minute, time.Second)
	d.egressDir = t.TempDir()

	found := []provider.Unit{{
		Sandbox: "osb-1", Service: "sandbox", EgressGateway: "172.30.0.1", EgressStat: addr,
	}}

	return d, fp, doors, found
}

// N13: another sandbox's network, created after this filter started, has a gateway on the VM the
// filter was never told about. The daemon pushes the engine's gateways on each tick; the filter
// refuses the new one from the next tick on.
func TestTheDaemonPushesANetworkCreatedLaterToEveryContainerFilter(t *testing.T) {
	d, fp, doors, found := newDoorsSetup(t, "tok")
	ctx := context.Background()
	later := netip.MustParseAddr("172.31.0.1")

	d.pushEgressDoors(ctx, found)

	if doors.Refuse(later) {
		t.Fatal("the fixture refuses the late gateway before it exists; the test proves nothing")
	}

	fp.mu.Lock()
	fp.doors = append(fp.doors, "172.31.0.1")
	fp.mu.Unlock()

	d.pushEgressDoors(ctx, found)

	if !doors.Refuse(later) {
		t.Fatalf("a network created after the filter started is not refused after a tick: %v", doors.Prefixes())
	}

	// A listing that fails pushes nothing: the last good set stays.
	fp.mu.Lock()
	fp.doors, fp.err = nil, errors.New("docker is not answering")
	fp.mu.Unlock()

	d.pushEgressDoors(ctx, found)

	if !doors.Refuse(later) {
		t.Error("a failed listing cleared the pushed set")
	}
}

// The token changes when the filter container is replaced. A cached endpoint that stops working
// is forgotten, and the next tick finds the new one.
func TestTheDaemonRelearnsAReplacedFiltersToken(t *testing.T) {
	d, fp, doors, found := newDoorsSetup(t, "stale")
	ctx := context.Background()

	fp.mu.Lock()
	fp.doors = append(fp.doors, "172.31.0.1")
	fp.mu.Unlock()

	d.pushEgressDoors(ctx, found)

	if doors.Refuse(netip.MustParseAddr("172.31.0.1")) {
		t.Fatal("a push with the wrong token was applied")
	}

	fp.f.Token = "tok"
	d.pushEgressDoors(ctx, found)

	if !doors.Refuse(netip.MustParseAddr("172.31.0.1")) {
		t.Fatal("the daemon never relearned the filter's token")
	}
}

// N15: /last is behind the token now, so the scraper has to present it, or no filtered sandbox is
// ever seen to be busy and every one of them is slept while it works.
func TestTheScraperPresentsTheFiltersToken(t *testing.T) {
	d, _, _, found := newDoorsSetup(t, "tok")

	d.scrapeEgress(context.Background(), found)

	d.mu.Lock()
	got := d.egressSeen["172.30.0.1"]
	d.mu.Unlock()

	if got != 42 {
		t.Fatalf("the scraper read %d, want 42: it did not get past the token", got)
	}
}

// listedDoorsProvider also lists its sandbox, so a whole discovery pass can run against it.
type listedDoorsProvider struct {
	*doorsProvider
	units []provider.Unit
}

func (p listedDoorsProvider) List(context.Context, string) ([]provider.Unit, error) {
	return p.units, nil
}

// The push is wired into discovery: a unit test of pushEgressDoors alone passes against a daemon
// that never calls it.
func TestADiscoveryPassPushesTheEnginesDoors(t *testing.T) {
	d, fp, doors, found := newDoorsSetup(t, "tok")

	fp.mu.Lock()
	fp.doors = append(fp.doors, "172.31.0.1")
	fp.mu.Unlock()

	d.provider = listedDoorsProvider{doorsProvider: fp, units: found}

	d.discover(context.Background())

	if !doors.Refuse(netip.MustParseAddr("172.31.0.1")) {
		t.Fatal("a discovery pass did not push the engine's doors to the container filter")
	}
}

// End to end, in process: traffic through a real filter moves its /last, the scraper reads it
// behind the token and stamps the sandbox's unit; a tick with no traffic stamps nothing; and a
// scraper holding the wrong token stamps nothing, so the token is what the idle signal rides on.
func TestFilterTrafficKeepsTheUnitAwakeThroughTheTokenGatedScrape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	host := strings.TrimPrefix(upstream.URL, "http://")

	f := egress.New([]string{"127.0.0.1", host})

	var last atomic.Int64
	f.OnActivity = func() { last.Store(time.Now().UnixNano()) }

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	for _, token := range []string{"tok", "wrong"} {
		ctl := httptest.NewServer(&egress.Control{Filter: f, Token: "tok", Last: last.Load})
		defer ctl.Close()

		addr := strings.TrimPrefix(ctl.URL, "http://")
		fp := &filterProvider{f: provider.EgressFilter{
			Sandbox: "osb-1", Gateway: "172.30.0.1", Services: []string{"sandbox"},
			Declared: declared(), Control: addr, Token: token,
		}}

		d := New(fp, time.Minute, time.Minute, time.Second)
		d.egressDir = t.TempDir()

		u := newUnit("osb-1", "sandbox", "ref", "i", "sandbox", nil, true)
		u.egressGateway = "172.30.0.1"
		u.lastByte.Store(1)
		d.units["ref"] = u

		found := []provider.Unit{{Sandbox: "osb-1", Service: "sandbox", EgressGateway: "172.30.0.1", EgressStat: addr}}

		pu, _ := url.Parse(proxy.URL)
		resp, err := (&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}).Get(upstream.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the filter did not carry the request: %d", resp.StatusCode)
		}

		d.scrapeEgress(context.Background(), found)
		stamped := u.lastByte.Load()

		if token == "wrong" {
			if stamped != 1 {
				t.Fatal("a scraper without the filter's token stamped the unit")
			}

			continue
		}

		if stamped == 1 {
			t.Fatal("traffic through the filter did not stamp the unit through the scrape")
		}

		// No traffic since: the reading has not moved, so the next tick must not stamp.
		u.lastByte.Store(1)
		d.scrapeEgress(context.Background(), found)

		if u.lastByte.Load() != 1 {
			t.Fatal("a tick with no traffic stamped the unit: nothing filtered would ever sleep")
		}
	}
}
