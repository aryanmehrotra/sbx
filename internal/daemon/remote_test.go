package daemon

// The dashboard, pointed at a deployment rather than at this machine.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

	mu         sync.Mutex
	samples    int
	started    []string
	stopped    []string
	removed    []string
	loggedRef  string
	limitedRef string
	limitedTo  provider.Limits
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

// The control path records what it was asked to do, so a test can assert the dashboard's key
// reached the provider on the far side rather than only that the HTTP call returned 200.
func (m *metered) Start(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, ref)

	return nil
}

func (m *metered) Stop(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = append(m.stopped, ref)

	return nil
}

func (m *metered) Remove(_ context.Context, sandbox string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, sandbox)

	return nil
}

func (m *metered) Logs(_ context.Context, ref string, _ int, _ bool, w io.Writer) error {
	m.mu.Lock()
	m.loggedRef = ref
	m.mu.Unlock()

	_, _ = io.WriteString(w, "line one\nline two\n")

	return nil
}

// Both halves, because provider.Limiter is the pair - a stub with only the getter is not a
// Limiter, the type assertion misses, and the endpoint reports no ceilings while looking
// entirely correct.
func (m *metered) SetLimits(_ context.Context, ref string, l provider.Limits) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limitedRef, m.limitedTo = ref, l

	return nil
}

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

// Exec, a TTY into a service and file copy stay refused over connect - a shell over the tunnel
// is a larger surface than the dashboard's keys and is not built here. The refusal names the
// door rather than the machine, because the same command works where the sandbox runs.
func TestAShellStaysRefusedOverConnect(t *testing.T) {
	r := remoteFor(t, "https://sbx.example.dev")

	ctx := context.Background()

	_, execErr := r.Exec(ctx, "ref", []string{"ls"})

	for name, err := range map[string]error{
		"Exec":    execErr,
		"ExecTTY": r.ExecTTY(ctx, "ref", []string{"sh"}),
		"Copy":    r.Copy(ctx, "ref", ":/a", "/b"),
	} {
		if err == nil {
			t.Errorf("%s was accepted over a connect endpoint", name)

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

// The whole point of the feature: a dashboard key over --connect does on the far side what it
// does locally. List first, because control routes by what the last listing returned.
func TestTheDashboardControlsADeployedFleet(t *testing.T) {
	d := daemonFronting(20000, "i1")
	m := &metered{}
	d.provider = m
	ts := serverFor(t, d)

	r := remoteFor(t, ts.URL)

	units, err := r.List(context.Background(), "")
	if err != nil || len(units) != 1 {
		t.Fatalf("List gave %d units, err %v", len(units), err)
	}

	ref, sandbox := units[0].Ref, units[0].Sandbox
	ctx := context.Background()

	if err := r.Stop(ctx, ref); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if err := r.Start(ctx, ref); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := r.SetLimits(ctx, ref, provider.Limits{NanoCPUs: 1_000_000_000, MemBytes: 512 << 20}); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	var logs bytes.Buffer
	if err := r.Logs(ctx, ref, 50, false, &logs); err != nil {
		t.Fatalf("Logs: %v", err)
	}

	if err := r.Remove(ctx, sandbox); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.stopped) != 1 || m.stopped[0] != "sbx-demo-db" {
		t.Errorf("sleep reached the provider as %v, want [sbx-demo-db]", m.stopped)
	}

	if len(m.started) != 1 || m.started[0] != "sbx-demo-db" {
		t.Errorf("wake reached the provider as %v", m.started)
	}

	if m.limitedRef != "sbx-demo-db" || m.limitedTo.MemBytes != 512<<20 {
		t.Errorf("limit reached the provider as ref=%q %+v", m.limitedRef, m.limitedTo)
	}

	if m.loggedRef != "sbx-demo-db" {
		t.Errorf("logs asked for ref %q", m.loggedRef)
	}

	if !strings.Contains(logs.String(), "line one") {
		t.Errorf("the log tail did not cross the wire: %q", logs.String())
	}

	if len(m.removed) != 1 || m.removed[0] != "demo" {
		t.Errorf("remove reached the provider as %v, want [demo]", m.removed)
	}
}

// Front mode has no runtime behind the endpoint, so control has nothing to act on. It must
// refuse clearly rather than 500, and say where the command would work.
func TestControlRefusesInFrontMode(t *testing.T) {
	d := daemonFronting(20000, "i1") // no provider set: this is front mode
	ts := serverFor(t, d)

	r := remoteFor(t, ts.URL)
	if _, err := r.List(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	err := r.Stop(context.Background(), "sbx-demo-db")
	if err == nil {
		t.Fatal("sleep was accepted against a front-mode endpoint with no runtime")
	}

	if !strings.Contains(err.Error(), "only fronts") {
		t.Errorf("the refusal does not explain there is no runtime: %v", err)
	}
}

// A control call with no token is refused by the same gate as everything else, so widening what
// the token buys did not open an unauthenticated door.
func TestControlNeedsTheToken(t *testing.T) {
	d := daemonFronting(20000, "i1")
	d.provider = &metered{}
	ts := serverFor(t, d)

	base := mustParse(t, ts.URL)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		base.String()+"/v1/control/sleep", strings.NewReader(`{"ref":"sbx-demo-db"}`))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a control call with no token answered %s, want 401", resp.Status)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Port-forward, end to end: a client on this machine reaches a service that is "elsewhere"
// (here, a loopback echo server the daemon fronts) through a port the dashboard bound. This is
// the whole point of the f key - no separate sbx connect, and the bytes arrive.
func TestForwardCarriesBytesToTheService(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()

	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}

			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()

	port := echo.Addr().(*net.TCPAddr).Port

	d := daemonFronting(port, "i1")
	d.provider = &metered{}
	ts := serverFor(t, d)

	r := remoteFor(t, ts.URL)

	units, err := r.List(context.Background(), "")
	if err != nil || len(units) == 0 {
		t.Fatalf("List gave %d units, err %v", len(units), err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	forwards, err := r.Forward(ctx, units[0].Ref)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}

	if len(forwards) != 1 {
		t.Fatalf("expected one forwarded port, got %d", len(forwards))
	}

	local := forwards[0].Local

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), 5*time.Second)
	if err != nil {
		t.Fatalf("could not reach the forwarded port %d: %v", local, err)
	}
	defer conn.Close()

	want := []byte("hello through the forward")
	if _, err := conn.Write(want); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("reading the echo back: %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("through the forward got %q, want %q", got, want)
	}

	again, err := r.Forward(ctx, units[0].Ref)
	if err != nil || len(again) != 1 || again[0].Local != local {
		t.Errorf("second Forward should return the same port %d, got %v (err %v)", local, again, err)
	}
}
