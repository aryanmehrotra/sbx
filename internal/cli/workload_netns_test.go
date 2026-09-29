package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// netnsStub is an image with no cat (scratch, distroless) on a provider that can read the
// container's network tables from a helper in its network namespace.
type netnsStub struct {
	*insideStub

	isolation provider.Isolation
	helperErr error
	helps     int
}

func (s *netnsStub) List(ctx context.Context, sandbox string) ([]provider.Unit, error) {
	units, err := s.insideStub.List(ctx, sandbox)
	for i := range units {
		units[i].Isolation = s.isolation
	}

	return units, err
}

func (s *netnsStub) NetTables(_ context.Context, ref string) (provider.NetTables, error) {
	s.mu.Lock()
	s.helps++
	s.mu.Unlock()

	if s.helperErr != nil {
		return provider.NetTables{}, s.helperErr
	}

	if ref != "sbx-x-redis" {
		return provider.NetTables{}, errors.New("asked about " + ref)
	}

	return provider.NetTables{Dev: s.dev, TCP: s.tcp, TCP6: s.tcp6}, nil
}

func noCatWithHelper(t *testing.T) *netnsStub {
	s := serving(t)
	s.noCat = true
	s.upstream = listening(t) // accepts and closes without a byte: the host dial refuses this

	return &netnsStub{insideStub: s}
}

// An image with no cat fell back to the dial from the host, which refuses a listener that accepts
// and closes without a byte - a scratch image running `nc -l </dev/null` behind `health: "true"`
// failed readiness while it served. Its network tables are read from a helper that shares its
// network namespace instead, which needs nothing from the image.
func TestReadyReadsAnImageWithNoCatThroughAHelper(t *testing.T) {
	p := noCatWithHelper(t)

	if err := Ready(context.Background(), p, "x", 600*time.Millisecond); err != nil {
		t.Fatalf("a scratch image listening on 0.0.0.0:6379 was refused: %v", err)
	}

	if p.helps == 0 {
		t.Fatal("the helper was never asked")
	}

	// And it judges, not just passes: nothing on the port is still refused, with the inside reason.
	p = noCatWithHelper(t)
	p.tcp = tcpHeader + tcpEstab6379

	err := Ready(context.Background(), p, "x", 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "nothing listens on 6379 inside the container") {
		t.Fatalf("want the inside verdict through the helper, got %v", err)
	}
}

// gVisor and Kata keep the workload's sockets in their own kernel: the namespace a helper can join
// on the host holds none of them, so it would say "nothing listens" of a serving sandbox. Those
// keep the host dial.
func TestReadyDoesNotUseTheHelperWhereItsNamespaceIsNotTheWorkloads(t *testing.T) {
	for _, iso := range []provider.Isolation{provider.IsolationGVisor, provider.IsolationKata} {
		p := noCatWithHelper(t)
		p.isolation = iso
		p.upstream = holding(t)

		if err := Ready(context.Background(), p, "x", 2*time.Second); err != nil {
			t.Errorf("%s: want the host dial's pass, got %v", iso, err)
		}

		if p.helps != 0 {
			t.Errorf("%s: the helper was asked %d time(s) about a namespace that is not the workload's", iso, p.helps)
		}
	}
}

// A helper that cannot run - no alpine:3 on an offline machine - is not a verdict about the
// workload: it falls back to the host dial, which is what an image with no cat always had.
func TestReadyFallsBackWhenTheHelperCannotRun(t *testing.T) {
	p := noCatWithHelper(t)
	p.upstream = holding(t)
	p.helperErr = errors.New("Unable to find image 'alpine:3' locally: dial tcp: lookup registry-1.docker.io: no such host")

	if err := Ready(context.Background(), p, "x", 2*time.Second); err != nil {
		t.Fatalf("want the host dial's pass, got %v", err)
	}
}
