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
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
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

	for _, bad := range []string{"db=nine", "db", "=5", "db=1000,cache"} {
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
