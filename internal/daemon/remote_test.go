package daemon

// The dashboard, pointed at a deployment rather than at this machine.

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// metered is a provider that can be sampled and can report ceilings, which is what a docker
// host is and what the connect endpoint passes through. Only the methods the endpoint calls are
// implemented; the embedded nil interface supplies the rest of the method set, and anything
// that reached them would panic loudly rather than quietly return a zero.
type metered struct {
	provider.Provider

	samples int
}

func (m *metered) Stats(_ context.Context, refs []string) (map[string]provider.Usage, error) {
	m.samples++

	out := map[string]provider.Usage{}
	for _, r := range refs {
		out[r] = provider.Usage{
			CPUNanos: 5_000_000_000, SystemNanos: 100_000_000_000, OnlineCPUs: 4,
			MemBytes: 300 << 20, MemLimit: 1 << 30,
		}
	}

	return out, nil
}

func (m *metered) Limits(context.Context, string) (provider.Limits, error) {
	return provider.Limits{NanoCPUs: 1_500_000_000, MemBytes: 640 << 20}, nil
}

// Both halves, because provider.Limiter is the pair - a stub with only the getter is not a
// Limiter, the type assertion misses, and the endpoint reports no ceilings while looking
// entirely correct.
func (m *metered) SetLimits(context.Context, string, provider.Limits) error { return nil }

func remoteFor(t *testing.T, urls ...string) *Remote {
	t.Helper()

	eps := make([]Endpoint, 0, len(urls))

	for _, u := range urls {
		label, raw := SplitEndpoint(u)
		eps = append(eps, Endpoint{Label: label, URL: raw, Token: testToken})
	}

	r, err := NewRemote(eps, nil)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

// A deployed sandbox has to arrive as ordinary rows, because the dashboard is written against a
// Provider and knows nothing about tunnels. This is the whole feature in one test: what the
// endpoint reports becomes units, usage and ceilings on this side.
func TestTheDashboardReadsADeployedFleet(t *testing.T) {
	d := daemonFronting(20000, "i1")
	d.provider = &metered{}
	ts := serverFor(t, d)

	r := remoteFor(t, ts.URL)

	units, err := r.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	if len(units) != 1 {
		t.Fatalf("a deployment fronting one service listed %d units", len(units))
	}

	u := units[0]
	if u.Sandbox != "demo" || u.Service != "db" || !u.Running {
		t.Errorf("the unit came back as %+v", u)
	}

	if len(u.Client) != 1 || u.Client[0].Port != 20000 {
		t.Errorf("the service's port did not survive the trip: %+v", u.Client)
	}

	// Sampled at the same moment as the listing, and answered from it.
	usage, err := r.Stats(context.Background(), []string{u.Ref})
	if err != nil {
		t.Fatal(err)
	}

	if got := usage[u.Ref]; got.MemBytes != 300<<20 || got.OnlineCPUs != 4 {
		t.Errorf("usage did not survive the trip: %+v", got)
	}

	lim, err := r.Limits(context.Background(), u.Ref)
	if err != nil {
		t.Fatal(err)
	}

	if lim.NanoCPUs != 1_500_000_000 || lim.MemBytes != 640<<20 {
		t.Errorf("the ceiling did not survive the trip: %+v", lim)
	}
}

// Counters rather than a percentage, so the rate is computed once - on the side that draws it -
// from two samples it chose. A percentage on the wire would mean the server picking the window
// and every client inheriting it.
func TestUsageCrossesTheWireAsCountersNotAPercentage(t *testing.T) {
	d := daemonFronting(20000, "i1")
	d.provider = &metered{}
	ts := serverFor(t, d)

	units, _ := remoteFor(t, ts.URL).List(context.Background(), "")
	if len(units) == 0 {
		t.Fatal("nothing listed")
	}

	r := remoteFor(t, ts.URL)
	_, _ = r.List(context.Background(), "")

	got, _ := r.Stats(context.Background(), []string{units[0].Ref})
	if u := got[units[0].Ref]; u.CPUNanos == 0 || u.SystemNanos == 0 {
		t.Errorf("cpu arrived without the counters it is computed from: %+v", u)
	}
}

// Sampling costs the deployment a round trip per running container. `sbx connect` wants a port
// map and asks for it a handful of times a session; the dashboard asks every couple of seconds.
// Charging the first for the second is how a listing that was instant becomes the slow part.
func TestConnectDoesNotPayForTheDashboardsSampling(t *testing.T) {
	d := daemonFronting(20000, "i1")
	m := &metered{}
	d.provider = m
	ts := serverFor(t, d)

	base, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	services, err := fetchFleet(context.Background(), base, testToken, false)
	if err != nil {
		t.Fatal(err)
	}

	if m.samples != 0 {
		t.Errorf("a plain fleet request sampled %d times; connect asks for a port map, not "+
			"a dashboard", m.samples)
	}

	for _, s := range services {
		if s.Usage != nil || s.Limits != nil {
			t.Errorf("a plain fleet request carried usage: %+v", s)
		}
	}

	if _, err := fetchFleet(context.Background(), base, testToken, true); err != nil {
		t.Fatal(err)
	}

	if m.samples != 1 {
		t.Errorf("asking for stats sampled %d times, want 1", m.samples)
	}
}

// The token on a connect endpoint buys reading what is fronted and carrying bytes to a port.
// Waking, sleeping, capping and removing are none of those, and a leaked token is a very
// different incident if it can destroy a volume. Every verb that changes something refuses, and
// says where the command does work.
func TestARemoteDashboardCannotChangeAnything(t *testing.T) {
	r := remoteFor(t, "https://sbx.example.dev")

	ctx := context.Background()

	for name, err := range map[string]error{
		"Start":     r.Start(ctx, "ref"),
		"Stop":      r.Stop(ctx, "ref"),
		"Remove":    r.Remove(ctx, "demo"),
		"SetLimits": r.SetLimits(ctx, "ref", provider.Limits{}),
		"Logs":      r.Logs(ctx, "ref", 10, false, nil),
		"ExecTTY":   r.ExecTTY(ctx, "ref", []string{"sh"}),
	} {
		if err == nil {
			t.Errorf("%s changed something through a read-only endpoint", name)

			continue
		}

		if !strings.Contains(err.Error(), "not available over sbx connect") {
			t.Errorf("%s refused with a message that does not say why: %v", name, err)
		}
	}
}

// Two deployments can each hold a service called "db". A ref is only unique within the daemon
// that issued it, and the dashboard keys history and ceilings by ref - so colliding refs would
// draw one deployment's trace under the other's name.
func TestTwoDeploymentsDoNotShareOneRef(t *testing.T) {
	a := daemonFronting(20000, "i1")
	a.provider = &metered{}

	b := daemonFronting(20000, "i2")
	b.provider = &metered{}

	tsA, tsB := serverFor(t, a), serverFor(t, b)

	r := remoteFor(t, "one="+tsA.URL, "two="+tsB.URL)

	units, err := r.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	if len(units) != 2 {
		t.Fatalf("two deployments each fronting a service gave %d units", len(units))
	}

	if units[0].Ref == units[1].Ref {
		t.Fatalf("both deployments' services arrived as %q, so one would draw over the other",
			units[0].Ref)
	}

	for _, u := range units {
		if !strings.HasPrefix(u.Ref, "one/") && !strings.HasPrefix(u.Ref, "two/") {
			t.Errorf("a ref does not say which deployment it came from: %q", u.Ref)
		}
	}
}

// One deployment being unreachable must not blank the screen. This is the opposite of what
// `sbx connect` does, and deliberately: a port map with a hole sends bytes to the wrong place,
// while a dashboard missing a row is a dashboard missing a row.
func TestOneUnreachableDeploymentStillShowsTheOthers(t *testing.T) {
	up := daemonFronting(20000, "i1")
	up.provider = &metered{}
	ts := serverFor(t, up)

	// A port nothing is listening on, on this machine, so it fails fast rather than hanging.
	r := remoteFor(t, "up="+ts.URL, "down=http://127.0.0.1:1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	units, err := r.List(ctx, "")
	if err != nil {
		t.Fatalf("one dead deployment failed the whole listing: %v", err)
	}

	if len(units) != 1 {
		t.Fatalf("expected the reachable deployment's one service, got %d units", len(units))
	}
}
