package daemon

// Several deployments, one local port map.
//
// A sandbox is a group of services, and a platform that gives one container per service spreads
// that group across several deployments. The client is where they are put back together - which
// is the whole reason not to cram several services into one container and lose the image the
// spec named.
//
// Every test here that carries bytes has to shift its ports. In production the workload's port
// is inside its own container, so the same number is free on the laptop; in a test the echo
// server and the local listener are on one machine and would fight over it. The shift is
// computed from a port that was just free, which is also how the per-deployment offset gets
// exercised.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// frontedAt is a deployment fronting one port, with no container runtime behind it.
func frontedAt(t *testing.T, port int, name string) *httptest.Server {
	t.Helper()

	d := New(nil, time.Minute, time.Minute, time.Minute)
	d.startupErr = errors.New("no docker daemon found")
	d.fronted = map[int]fronted{port: {name: name, port: port}}

	return serverFor(t, d)
}

// syncBuffer is Out for a Connect that is still running.
//
// The report is written on Connect's goroutine and read on the test's, which is a data race
// unless the buffer holds a lock - the same bug the dashboard's tests had, and the detector
// fails the build over it either way.
type syncBuffer struct {
	mu    sync.Mutex
	b     bytes.Buffer
	first time.Time // when the report appeared, which is when the fetching finished
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.first.IsZero() {
		s.first = time.Now()
	}

	return s.b.Write(p)
}

func (s *syncBuffer) firstWrite() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.first
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// runConnect starts Connect and waits until its listeners are up, or fails the test.
func runConnect(t *testing.T, opt ClientOptions, wantLocal ...int) (*syncBuffer, func()) {
	t.Helper()

	out := &syncBuffer{}
	opt.Out = out

	ctx, cancel := context.WithCancel(context.Background())

	var (
		wg  sync.WaitGroup
		err error
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		err = Connect(ctx, opt)
	}()

	stop := func() {
		cancel()
		wg.Wait()

		if err != nil {
			t.Errorf("Connect returned %v", err)
		}
	}

	for _, port := range wantLocal {
		if !waitDialable(port) {
			cancel()
			wg.Wait()
			t.Fatalf("127.0.0.1:%d never opened (Connect said: %v)\n%s", port, err, out)
		}
	}

	return out, stop
}

func waitDialable(port int) bool {
	for range 200 {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = c.Close()

			return true
		}

		time.Sleep(10 * time.Millisecond)
	}

	return false
}

// echoThrough writes to a local port and expects the far echo server to answer.
func echoThrough(t *testing.T, port int, msg string) {
	t.Helper()

	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dialling 127.0.0.1:%d: %v", port, err)
	}

	defer c.Close()

	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("writing to 127.0.0.1:%d: %v", port, err)
	}

	buf := make([]byte, len(msg))
	if _, err := readFull(c, buf); err != nil {
		t.Fatalf("reading back from 127.0.0.1:%d: %v", port, err)
	}

	if string(buf) != msg {
		t.Errorf("127.0.0.1:%d echoed %q, want %q", port, buf, msg)
	}
}

func readFull(c net.Conn, buf []byte) (int, error) {
	got := 0

	for got < len(buf) {
		n, err := c.Read(buf[got:])
		got += n

		if err != nil {
			return got, err
		}
	}

	return got, nil
}

// The point of the whole feature: two deployments, one command, and both are just local ports.
func TestTwoDeploymentsBecomeOneLocalPortMap(t *testing.T) {
	dbPort, cachePort := echoPort(t), echoPort(t)
	db := frontedAt(t, dbPort, "db")
	cache := frontedAt(t, cachePort, "cache")

	dbLocal, cacheLocal := freePort(t), freePort(t)

	out, stop := runConnect(t, ClientOptions{
		Endpoints: []Endpoint{
			{Label: "db", URL: db.URL, Token: testToken},
			{Label: "cache", URL: cache.URL, Token: testToken},
		},
		Offsets: map[string]int{"db": dbLocal - dbPort, "cache": cacheLocal - cachePort},
	}, dbLocal, cacheLocal)

	defer stop()

	// Bytes reach the right one. Different payloads, because two tunnels crossed would still
	// echo something.
	echoThrough(t, dbLocal, "hello from the database side")
	echoThrough(t, cacheLocal, "and this one is the cache")

	report := out.String()
	for _, want := range []string{"2 deployments", "db", "cache", db.URL, cache.URL} {
		if !strings.Contains(report, want) {
			t.Errorf("the report never mentions %q:\n%s", want, report)
		}
	}
}

// Two postgres deployments both front 5432. Binding the second one has to say who already has
// the port - the failure is otherwise reported as though something outside sbx took it.
func TestTwoDeploymentsWantingOnePortSayWhichTwo(t *testing.T) {
	port := freePort(t)
	db := frontedAt(t, port, "db")
	other := frontedAt(t, port, "db")

	err := Connect(context.Background(), ClientOptions{
		Endpoints: []Endpoint{
			{Label: "main", URL: db.URL, Token: testToken},
			{Label: "replica", URL: other.URL, Token: testToken},
		},
		Out: &bytes.Buffer{},
	})

	if err == nil {
		t.Fatal("two deployments fronting one port connected anyway")
	}

	for _, want := range []string{"main", "replica", "--port-offset"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the clash never mentions %q: %v", want, err)
		}
	}
}

// ... and moving one of them is how it is fixed, which is what the message told them to do.
func TestAPerDeploymentOffsetResolvesTheClash(t *testing.T) {
	port := echoPort(t)
	db := frontedAt(t, port, "db")
	other := frontedAt(t, port, "db")

	mainLocal, replicaLocal := freePort(t), freePort(t)

	_, stop := runConnect(t, ClientOptions{
		Endpoints: []Endpoint{
			{Label: "main", URL: db.URL, Token: testToken},
			{Label: "replica", URL: other.URL, Token: testToken},
		},
		Offsets: map[string]int{"main": mainLocal - port, "replica": replicaLocal - port},
	}, mainLocal, replicaLocal)

	defer stop()

	echoThrough(t, mainLocal, "main")
	echoThrough(t, replicaLocal, "replica")
}

// One deployment down fails the command. A port map with holes in it is the case where somebody
// reaches their own laptop's sandbox believing they reached the remote one.
func TestOneUnreachableDeploymentFailsTheWholeCommand(t *testing.T) {
	port := echoPort(t)
	up := frontedAt(t, port, "db")

	dead := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))

	err := Connect(context.Background(), ClientOptions{
		Endpoints: []Endpoint{
			{Label: "db", URL: up.URL, Token: testToken},
			{Label: "cache", URL: dead, Token: testToken},
		},
		Offsets: map[string]int{"db": freePort(t) - port},
		Out:     &bytes.Buffer{},
	})

	if err == nil {
		t.Fatal("connected with one deployment unreachable")
	}

	if !strings.Contains(err.Error(), "could not reach") {
		t.Errorf("error = %v, want it to say which one could not be reached", err)
	}
}

// A single deployment keeps the output it always had - the common case should not pay for the
// new one.
func TestOneDeploymentStillReadsAsOne(t *testing.T) {
	port := echoPort(t)
	db := frontedAt(t, port, "db")
	local := freePort(t)

	out, stop := runConnect(t, ClientOptions{
		Endpoints: []Endpoint{{URL: db.URL, Token: testToken}},
		Offset:    local - port,
	}, local)

	defer stop()

	if got := out.String(); !strings.Contains(got, "sbx connect · "+db.URL) ||
		strings.Contains(got, "deployments") {
		t.Errorf("one deployment reported as though there were several:\n%s", got)
	}
}

// An offset that lands on port 0 is the dangerous one: net.Listen reads 0 as "any free port",
// so it succeeds, binds something random, and the listing still says :0. The service is then
// unreachable at the only address the user was given.
func TestAnOffsetOntoPortZeroIsRefused(t *testing.T) {
	port := freePort(t)
	db := frontedAt(t, port, "db")

	for name, shift := range map[string]int{
		"onto zero":  -port,
		"below zero": -port - 5,
		"past 65535": 65536 - port,
	} {
		err := Connect(context.Background(), ClientOptions{
			Endpoints: []Endpoint{{Label: "db", URL: db.URL, Token: testToken}},
			Offsets:   map[string]int{"db": shift},
			Out:       &syncBuffer{},
		})

		if err == nil {
			t.Errorf("%s: an offset to %d was accepted", name, port+shift)

			continue
		}

		if !strings.Contains(err.Error(), "not a port") {
			t.Errorf("%s: error = %v, want it to say the offset left the port range", name, err)
		}
	}
}

// db-1 and db_1 look like two deployments and are one environment variable, so the second
// would silently borrow the first one's token.
func TestLabelsThatCollideAsOneVariableAreRefused(t *testing.T) {
	err := Connect(context.Background(), ClientOptions{
		Endpoints: []Endpoint{
			{Label: "db-1", URL: "https://a.example.dev", Token: "t"},
			{Label: "db_1", URL: "https://b.example.dev", Token: "t"},
		},
	})

	if err == nil {
		t.Fatal("two labels sharing one token variable were accepted")
	}

	if !strings.Contains(err.Error(), "SBX_CONNECT_TOKEN_DB_1") {
		t.Errorf("error = %v, want it to name the variable they share", err)
	}
}

// The deployments wake on the first request, so asking them in turn would pay a cold start
// once per deployment. This pins that they are asked together.
func TestDeploymentsAreAskedAtTheSameTime(t *testing.T) {
	const delay = 300 * time.Millisecond

	slow := func(label string) Endpoint {
		port := freePort(t)
		d := New(nil, time.Minute, time.Minute, time.Minute)
		d.startupErr = errors.New("no docker daemon found")
		d.fronted = map[int]fronted{port: {name: label, port: port}}

		srv, err := d.Connect(ConnectOptions{Addr: "127.0.0.1:0", Token: testToken})
		if err != nil {
			t.Fatal(err)
		}

		handler := srv.Handler
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			handler.ServeHTTP(w, r)
		}))

		t.Cleanup(ts.Close)

		return Endpoint{Label: label, URL: ts.URL, Token: testToken}
	}

	eps := []Endpoint{slow("a"), slow("b"), slow("c")}

	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	start := time.Now()

	go func() { done <- Connect(ctx, ClientOptions{Endpoints: eps, Out: out}) }()

	// The report is written once every fleet is in, so its timestamp is when the asking
	// finished. Connect itself runs until the context ends, which is not what is being timed.
	deadline := time.Now().Add(10 * time.Second)
	for out.firstWrite().IsZero() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	took := out.firstWrite().Sub(start)

	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if took <= 0 {
		t.Fatal("the report never arrived")
	}

	if took > 2*delay {
		t.Errorf("three deployments took %v to ask, which is one after another rather than "+
			"together (each answers in %v)", took, delay)
	}
}

// An empty deployment has two possible reasons and they are not interchangeable. Saying
// "nothing matches --sandbox" to somebody who never passed --sandbox sends them looking for a
// filter they did not use, while the real answer is that the deployment is fronting nothing.
func TestAnEmptyDeploymentSaysWhyItIsEmpty(t *testing.T) {
	full := echoPort(t)
	db := frontedAt(t, full, "db")

	// A deployment that came up fine and simply has nothing in it yet. Not the no-runtime case:
	// that one answers 503 on purpose, because "no sandboxes" and "I cannot see any sandboxes"
	// are different answers and it refuses to give the first when it means the second.
	empty := New(nil, time.Minute, time.Minute, time.Minute)
	empty.fronted = map[int]fronted{}
	quiet := serverFor(t, empty)

	eps := []Endpoint{
		{Label: "db", URL: db.URL, Token: testToken},
		{Label: "quiet", URL: quiet.URL, Token: testToken},
	}

	// No --sandbox: the honest answer is that it fronts nothing.
	local := freePort(t)
	out, stop := runConnect(t, ClientOptions{
		Endpoints: eps,
		Offsets:   map[string]int{"db": local - full},
	}, local)

	got := out.String()
	stop()

	if strings.Contains(got, "--sandbox") {
		t.Errorf("blamed --sandbox when it was never passed:\n%s", got)
	}

	if !strings.Contains(got, "fronting nothing") {
		t.Errorf("an empty deployment did not say it was empty:\n%s", got)
	}

	// Even with --sandbox in the command, this deployment's emptiness is not the filter's
	// doing: it had nothing to filter. Asserting on the line under `quiet` rather than on the
	// whole report, because the filter message is legitimately somewhere else in it.
	local2 := freePort(t)
	out2, stop2 := runConnect(t, ClientOptions{
		Endpoints: eps,
		Sandbox:   []string{"db"},
		Offsets:   map[string]int{"db": local2 - full},
	}, local2)

	got2 := out2.String()
	stop2()

	if under(got2, "quiet") != "fronting nothing" {
		t.Errorf("--sandbox was blamed for a deployment that was empty anyway: %q\n%s",
			under(got2, "quiet"), got2)
	}
}

// under returns the first line beneath the group header naming this deployment.
//
// Anchored at the start of the line rather than searched for anywhere in it: "cache-db · ..."
// contains "db · ", so a substring match reads the wrong deployment's line and the assertion
// silently checks something else. Nothing here collides today, which is exactly when a trap
// like that gets laid.
func under(report, label string) string {
	lines := strings.Split(report, "\n")

	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), label+" · ") && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}

	return ""
}

// The helper above is load-bearing for the assertions that tell the two kinds of empty apart,
// so it gets its own test rather than being trusted.
func TestUnderReadsTheRightDeploymentsLine(t *testing.T) {
	report := strings.Join([]string{
		"sbx connect · 2 deployments",
		"",
		"  cache-db · http://a",
		"    nothing here matches --sandbox",
		"",
		"  db · http://b",
		"    127.0.0.1:5432  db/postgres",
	}, "\n")

	if got := under(report, "db"); got != "127.0.0.1:5432  db/postgres" {
		t.Errorf("under(report, \"db\") = %q, want db's own line and not cache-db's", got)
	}
}

// A deployment that is fronting a service the filter excluded IS the filter's doing, and
// saying so is the whole point of telling them apart.
func TestAFilteredOutDeploymentNamesTheFilter(t *testing.T) {
	dbPort, cachePort := echoPort(t), echoPort(t)
	db := frontedAt(t, dbPort, "db")
	cache := frontedAt(t, cachePort, "cache")

	local := freePort(t)

	out, stop := runConnect(t, ClientOptions{
		Endpoints: []Endpoint{
			{Label: "db", URL: db.URL, Token: testToken},
			{Label: "cache", URL: cache.URL, Token: testToken},
		},
		Sandbox: []string{"db"},
		Offsets: map[string]int{"db": local - dbPort, "cache": freePort(t) - cachePort},
	}, local)

	got := out.String()
	stop()

	if under(got, "cache") != "nothing here matches --sandbox" {
		t.Errorf("a deployment emptied by --sandbox did not say so: %q\n%s",
			under(got, "cache"), got)
	}
}

// Nothing anywhere, and no filter: the error must not send somebody looking for a --sandbox
// they never passed.
func TestEverythingEmptyDoesNotBlameAFilterThatWasNotUsed(t *testing.T) {
	empty := New(nil, time.Minute, time.Minute, time.Minute)
	empty.fronted = map[int]fronted{}
	quiet := serverFor(t, empty)

	eps := []Endpoint{{Label: "quiet", URL: quiet.URL, Token: testToken}}

	err := Connect(context.Background(), ClientOptions{Endpoints: eps, Out: &syncBuffer{}})
	if err == nil {
		t.Fatal("a deployment fronting nothing connected anyway")
	}

	if strings.Contains(err.Error(), "matches") {
		t.Errorf("error = %q, want it not to imply a filter was applied", err)
	}

	// Passing --sandbox does not make an empty deployment the filter's fault. There was nothing
	// for it to exclude, so the honest answer is the same one as above.
	err = Connect(context.Background(), ClientOptions{
		Endpoints: eps, Sandbox: []string{"nope"}, Out: &syncBuffer{},
	})

	if err == nil {
		t.Fatal("a deployment fronting nothing connected anyway")
	}

	if strings.Contains(err.Error(), "--sandbox") {
		t.Errorf("error = %q, blames --sandbox for a deployment that had nothing to filter", err)
	}
}

// Two deployments can be empty for two different reasons at once, and one sentence covering
// both can only be right about one of them. This is the case that survived four rounds of
// fixing the same mistake somewhere else.
func TestEachEmptyDeploymentGetsItsOwnReason(t *testing.T) {
	port := echoPort(t)
	db := frontedAt(t, port, "db") // has a service, which --sandbox will exclude

	bare := New(nil, time.Minute, time.Minute, time.Minute)
	bare.fronted = map[int]fronted{}
	quiet := serverFor(t, bare) // has nothing, filter or no filter

	err := Connect(context.Background(), ClientOptions{
		Endpoints: []Endpoint{
			{Label: "quiet", URL: quiet.URL, Token: testToken},
			{Label: "db", URL: db.URL, Token: testToken},
		},
		Sandbox: []string{"nope"},
		Out:     &syncBuffer{},
	})

	if err == nil {
		t.Fatal("two empty deployments connected anyway")
	}

	line := func(label string) string {
		for _, l := range strings.Split(err.Error(), "\n") {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, label+" ") {
				return l
			}
		}

		return ""
	}

	if got := line("quiet"); got != "quiet is fronting nothing" {
		t.Errorf("quiet: %q - it was empty with or without the filter", got)
	}

	if got := line("db"); !strings.Contains(got, "--sandbox nope") {
		t.Errorf("db: %q - the filter really is why this one is empty", got)
	}
}

// ... but where the filter really is what emptied it, the error has to say so, or the fix
// above would just be silence in a different shape.
func TestAFilterThatExcludesEverythingSaysSo(t *testing.T) {
	port := echoPort(t)
	db := frontedAt(t, port, "db")

	err := Connect(context.Background(), ClientOptions{
		Endpoints: []Endpoint{{Label: "db", URL: db.URL, Token: testToken}},
		Sandbox:   []string{"nope"},
		Out:       &syncBuffer{},
	})

	if err == nil {
		t.Fatal("a filter matching nothing connected anyway")
	}

	if !strings.Contains(err.Error(), "--sandbox nope") {
		t.Errorf("error = %q, want it to name the filter that excluded everything", err)
	}
}

func TestSplitEndpoint(t *testing.T) {
	for arg, want := range map[string][2]string{
		"https://sbx.example.dev":     {"", "https://sbx.example.dev"},
		"db=https://sbx.example.dev":  {"db", "https://sbx.example.dev"},
		"http://h/p?a=b":              {"", "http://h/p?a=b"},
		"=https://sbx.example.dev":    {"", "=https://sbx.example.dev"},
		"my-db=http://127.0.0.1:8080": {"my-db", "http://127.0.0.1:8080"},
	} {
		label, raw := SplitEndpoint(arg)
		if label != want[0] || raw != want[1] {
			t.Errorf("SplitEndpoint(%q) = %q, %q; want %q, %q", arg, label, raw, want[0], want[1])
		}
	}
}

func TestParseOffsets(t *testing.T) {
	if every, by, err := ParseOffsets("1000"); err != nil || every != 1000 || by != nil {
		t.Errorf(`ParseOffsets("1000") = %d, %v, %v`, every, by, err)
	}

	if every, by, err := ParseOffsets(""); err != nil || every != 0 || by != nil {
		t.Errorf(`ParseOffsets("") = %d, %v, %v`, every, by, err)
	}

	every, by, err := ParseOffsets("db=1000, cache=2000")
	if err != nil || every != 0 || by["db"] != 1000 || by["cache"] != 2000 {
		t.Errorf(`ParseOffsets("db=1000, cache=2000") = %d, %v, %v`, every, by, err)
	}

	// Everything moves by 1000 except the one that was named. Without this, naming a single
	// deployment's offset silently resets every other deployment's to zero - back onto the
	// ports they were being moved away from.
	every, by, err = ParseOffsets("1000,replica=2000")
	if err != nil || every != 1000 || by["replica"] != 2000 {
		t.Errorf(`ParseOffsets("1000,replica=2000") = %d, %v, %v`, every, by, err)
	}

	for _, bad := range []string{
		"db=nine", "db", "=5", "db=1000,cache",
		"db=1,db=2", // one deployment, two answers
		"10,20",     // two defaults
	} {
		if _, _, err := ParseOffsets(bad); err == nil {
			t.Errorf("ParseOffsets(%q) was accepted", bad)
		}
	}
}

func TestTokenVar(t *testing.T) {
	for label, want := range map[string]string{
		"db":     "SBX_CONNECT_TOKEN_DB",
		"my-db":  "SBX_CONNECT_TOKEN_MY_DB",
		"a.b.c":  "SBX_CONNECT_TOKEN_A_B_C",
		"cache1": "SBX_CONNECT_TOKEN_CACHE1",
	} {
		if got := TokenVar(label); got != want {
			t.Errorf("TokenVar(%q) = %q, want %q", label, got, want)
		}
	}
}

// The label decides which token is read and what every message calls it, so two of them being
// the same is ambiguous rather than merely untidy.
func TestTwoDeploymentsCannotShareALabel(t *testing.T) {
	err := Connect(context.Background(), ClientOptions{
		Endpoints: []Endpoint{
			{Label: "db", URL: "https://a.example.dev", Token: "t"},
			{Label: "db", URL: "https://b.example.dev", Token: "t"},
		},
	})

	if err == nil || !strings.Contains(err.Error(), "both called") {
		t.Errorf("duplicate labels gave %v", err)
	}
}

// An offset for a deployment that was not given is a typo, and ignoring it would connect on
// exactly the ports the user was trying to move off.
func TestAnOffsetForNobodyIsRefused(t *testing.T) {
	err := Connect(context.Background(), ClientOptions{
		Endpoints: []Endpoint{{Label: "db", URL: "https://a.example.dev", Token: "t"}},
		Offsets:   map[string]int{"cache": 1000},
	})

	if err == nil || !strings.Contains(err.Error(), "cache") {
		t.Errorf("an offset naming an absent deployment gave %v", err)
	}
}

// Two deployments usually have two tokens: the message has to name the variable that holds
// this one, not the shared one somebody already set.
func TestAMissingTokenNamesItsOwnVariable(t *testing.T) {
	err := Connect(context.Background(), ClientOptions{
		Endpoints: []Endpoint{{Label: "cache", URL: "https://a.example.dev"}},
	})

	if err == nil || !strings.Contains(err.Error(), "SBX_CONNECT_TOKEN_CACHE") {
		t.Errorf("a missing token for a named deployment gave %v", err)
	}
}

func TestNoDeploymentAtAll(t *testing.T) {
	if err := Connect(context.Background(), ClientOptions{}); err == nil {
		t.Error("Connect with no endpoints succeeded")
	}
}

// --sandbox is a filter over everything that was found, not over one deployment.
func TestSandboxFilterAppliesAcrossDeployments(t *testing.T) {
	dbPort, cachePort := echoPort(t), echoPort(t)
	db := frontedAt(t, dbPort, "db")
	cache := frontedAt(t, cachePort, "cache")

	dbLocal := freePort(t)

	out, stop := runConnect(t, ClientOptions{
		Endpoints: []Endpoint{
			{Label: "db", URL: db.URL, Token: testToken},
			{Label: "cache", URL: cache.URL, Token: testToken},
		},
		Sandbox: []string{"db"},
		Offsets: map[string]int{"db": dbLocal - dbPort, "cache": freePort(t) - cachePort},
	}, dbLocal)

	defer stop()

	if got := out.String(); strings.Contains(got, "cache/fronted") {
		t.Errorf("--sandbox db still opened the cache:\n%s", got)
	}
}
