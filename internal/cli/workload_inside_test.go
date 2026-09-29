package cli

// Whether a workload is serving, read inside its container rather than dialled from outside.
//
// A dial from the host was wrong in both directions. Kata on a nested colima host: the guest has
// no network, and docker's forwarder holds a silent connection open for about 10 s (measured:
// `(sleep 10 | nc 127.0.0.1 30000)` held 9.6 s, three of three), so a silent line read as a
// server waiting for its client and ready said serving. And a real listener that accepts and
// closes without a byte (`nc -l </dev/null`) read as nothing there, so create failed a sandbox
// that works. Two facts from inside the container decide it instead, with no protocol bytes:
// does something listen on the declared port where outside can reach it, and does the container
// have a network interface besides loopback.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

const (
	devHeader = "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"
	devLoop = "    lo:     100       1    0    0    0     0          0         0      100       1    0    0    0     0       0          0\n"
	devEth  = "  eth0:    2000      20    0    0    0     0          0         0     1000      10    0    0    0     0       0          0\n"

	tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	// 0x18EB is 6379.
	tcpAny6379   = "   0: 00000000:18EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 1 1 0 100 0 0 10 0\n"
	tcpLocal6379 = "   0: 0100007F:18EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 1 1 0 100 0 0 10 0\n"
	// A connection FROM port 6379, not a listener on it: state 01, established.
	tcpEstab6379 = "   1: 0200000A:18EB 0300000A:D431 01 00000000:00000000 00:00000000 00000000   999        0 2 1 0 20 4 30 10 -1\n"
	tcp6Any6379  = "   0: 00000000000000000000000000000000:18EB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 3 1 0 100 0 0 10 0\n"
	tcp6Loop6379 = "   0: 00000000000000000000000001000000:18EB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 4 1 0 100 0 0 10 0\n"
)

// insideStub is one running, healthy local service whose container answers Exec with whatever
// the test puts in its /proc files.
type insideStub struct {
	provider.Provider

	daemon, upstream, private int

	dev, tcp, tcp6 string
	tcp6Missing    bool  // a kernel with IPv6 off has no /proc/net/tcp6
	noCat          bool  // an image with no cat: distroless, scratch
	execErr        error // every exec fails with this

	mu    sync.Mutex
	execs int
}

func (s *insideStub) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	return []provider.Unit{{
		Sandbox: sandbox, Service: "redis", Ref: "sbx-x-redis", Running: true,
		Client:   []provider.Endpoint{{Host: "127.0.0.1", Port: s.daemon}},
		Listen:   []int{s.daemon},
		Upstream: []provider.Endpoint{{Host: "127.0.0.1", Port: s.upstream}},
		Private:  []int{s.private},
	}}, nil
}

func (s *insideStub) Healthy(context.Context, string) (bool, bool) { return true, true }
func (s *insideStub) Probe(context.Context, string) (bool, bool)   { return true, true }

func (s *insideStub) Exec(_ context.Context, _ string, argv []string) (string, error) {
	s.mu.Lock()
	s.execs++
	s.mu.Unlock()

	switch {
	case s.execErr != nil:
		return "", s.execErr
	case s.noCat:
		return "", errors.New(`docker exec sbx-x-redis cat: exit status 127: OCI runtime exec failed: exec failed: ` +
			`unable to start container process: exec: "cat": executable file not found in $PATH: unknown`)
	case len(argv) != 2 || argv[0] != "cat":
		return "", errors.New("unexpected exec " + strings.Join(argv, " "))
	}

	switch argv[1] {
	case "/proc/net/dev":
		return s.dev, nil
	case "/proc/net/tcp":
		return s.tcp, nil
	case "/proc/net/tcp6":
		if s.tcp6Missing {
			return "", errors.New("docker exec: exit status 1: cat: can't open '/proc/net/tcp6': No such file or directory")
		}

		return s.tcp6, nil
	}

	return "", errors.New("no such file " + argv[1])
}

// serving is a redis container as it should be: eth0, and a listener on 0.0.0.0:6379.
func serving(t *testing.T) *insideStub {
	return &insideStub{daemon: holding(t), upstream: holding(t), private: 6379,
		dev: devHeader + devLoop + devEth, tcp: tcpHeader + tcpAny6379, tcp6: tcpHeader}
}

func TestReadyReadsTheWorkloadFromInsideTheContainer(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*insideStub)
		ok    bool
		says  []string
	}{
		{"listening on 0.0.0.0 with eth0", func(*insideStub) {}, true, nil},
		{"a listener that accepts and closes without a byte is serving (nc -l </dev/null)",
			func(s *insideStub) { s.upstream = listening(t) }, true, nil},
		{"listening on :: only (tcp6)", func(s *insideStub) { s.tcp, s.tcp6 = tcpHeader, tcpHeader+tcp6Any6379 }, true, nil},
		{"no /proc/net/tcp6 (IPv6 off) is not an error", func(s *insideStub) { s.tcp6Missing = true }, true, nil},

		{"nothing listens", func(s *insideStub) { s.tcp = tcpHeader + tcpEstab6379 }, false,
			[]string{"redis", "nothing listens on 6379 inside the container", "sbx logs x redis"}},
		{"listens only on 127.0.0.1", func(s *insideStub) { s.tcp = tcpHeader + tcpLocal6379 }, false,
			[]string{"redis", "only on 127.0.0.1", "0.0.0.0"}},
		{"listens only on ::1", func(s *insideStub) { s.tcp, s.tcp6 = tcpHeader, tcpHeader+tcp6Loop6379 }, false,
			[]string{"only on ::1"}},
		{"no network interface but loopback", func(s *insideStub) { s.dev = devHeader + devLoop }, false,
			[]string{"redis", "no network interface but loopback", "Kata"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := serving(t)
			c.setup(p)

			err := Ready(context.Background(), p, "x", 600*time.Millisecond)
			if c.ok {
				if err != nil {
					t.Fatalf("want serving, got %v", err)
				}

				return
			}

			if err == nil {
				t.Fatal("said serving")
			}

			for _, want := range c.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not say %q: %v", want, err)
				}
			}

			// The Kata line is for the case it describes: a service that never binds, or binds
			// to loopback, is a different problem, and the hint sends its reader the wrong way.
			if c.name != "no network interface but loopback" && strings.Contains(err.Error(), "Kata") {
				t.Errorf("the guest-network hint is on a failure it does not describe: %v", err)
			}
		})
	}
}

// Could not ask is not an answer: it keeps asking until the deadline, never passes, and says why.
func TestReadyDoesNotPassWhenTheContainerCannotBeAsked(t *testing.T) {
	p := serving(t)
	p.execErr = errors.New("Error response from daemon: container sbx-x-redis is paused, unpause the container before exec")

	err := Ready(context.Background(), p, "x", 700*time.Millisecond)
	if err == nil {
		t.Fatal("passed a container it could not ask")
	}

	for _, want := range []string{"could not ask", "is paused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}

	if p.execs < 2 {
		t.Errorf("asked %d time(s); a failed exec is asked again until the deadline", p.execs)
	}
}

// An image with no cat - distroless, scratch, a lone Go binary - can never be asked, and refusing
// on that would fail every ready of such an image forever. The dial from the host decides for it,
// with that dial's limits.
func TestReadyFallsBackToTheHostDialWhenTheImageHasNoCat(t *testing.T) {
	p := serving(t)
	p.noCat = true

	if err := Ready(context.Background(), p, "x", 2*time.Second); err != nil {
		t.Fatalf("an image without cat, behind a port that holds, was refused: %v", err)
	}

	p = serving(t)
	p.noCat, p.upstream = true, listening(t)

	if err := Ready(context.Background(), p, "x", 500*time.Millisecond); err == nil || !strings.Contains(err.Error(), "closed it") {
		t.Fatalf("the fallback dial should judge an accept-and-close port, got %v", err)
	}
}

// A container the runtime has not said the inside port of is dialled from the host, as before.
func TestReadyFallsBackToTheHostDialWithoutTheInsidePort(t *testing.T) {
	p := serving(t)
	p.private, p.upstream = 0, listening(t)

	err := Ready(context.Background(), p, "x", 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "closed it") {
		t.Fatalf("want the host dial's verdict, got %v", err)
	}

	if p.execs != 0 {
		t.Errorf("asked the container %d time(s) without knowing which port to look for", p.execs)
	}
}

// Create asks the same question: `nc -l </dev/null` behind `health: "true"` made create exit 1
// blaming the network, for a sandbox that works.
func TestCreatePassesAListenerThatAcceptsAndCloses(t *testing.T) {
	p := serving(t)
	p.upstream = listening(t)

	if err := checkCreatedWorkloads(context.Background(), p, "x", []string{"redis"}, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("create refused a listener that accepts and closes: %v", err)
	}
}

// A service listening on the wrong port was reported as "nothing listens on 6379", which sends its
// reader after a process that is not running when the fix is a port number. Say what it does
// listen on - only where outside can reach it, since a loopback listener is not the answer either.
func TestReadyNamesThePortsTheWorkloadDoesListenOn(t *testing.T) {
	const (
		// 0x2382 is 9090 on the wildcard; 0x1F90 is 8080 on 127.0.0.1 only; 0x0050 is 80 on :: .
		tcpAny9090   = "   0: 00000000:2382 00000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 5 1 0 100 0 0 10 0\n"
		tcpLocal8080 = "   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 6 1 0 100 0 0 10 0\n"
		tcp6Any80    = "   0: 00000000000000000000000000000000:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 7 1 0 100 0 0 10 0\n"
	)

	p := serving(t)
	p.tcp = tcpHeader + tcpAny9090 + tcpLocal8080 + tcpEstab6379
	p.tcp6 = tcpHeader + tcp6Any80

	err := Ready(context.Background(), p, "x", 500*time.Millisecond)
	if err == nil {
		t.Fatal("said serving with nothing on the declared port")
	}

	for _, want := range []string{"nothing listens on 6379 inside the container", "it listens on 80, 9090"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}

	if strings.Contains(err.Error(), "8080") {
		t.Errorf("a loopback-only listener was offered as where it listens: %v", err)
	}

	// Nothing listening anywhere reachable: no list, not an empty one.
	p = serving(t)
	p.tcp = tcpHeader + tcpEstab6379 + tcpLocal8080

	err = Ready(context.Background(), p, "x", 500*time.Millisecond)
	if err == nil || strings.Contains(err.Error(), "it listens on") {
		t.Fatalf("with no reachable listener the error should not name any: %v", err)
	}
}
