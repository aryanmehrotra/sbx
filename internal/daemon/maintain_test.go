package daemon

import (
	"context"
	"io"
	"log"
	"testing"
)

type maintainedProvider struct {
	listingProvider
	calls int
}

func (p *maintainedProvider) Maintain(context.Context) { p.calls++ }

// Every discovery pass gives the provider its chance to put back host state that drifted - a
// microVM bridge's firewall rules after a reload - so the daemon's reconcile repairs them.
func TestDiscoveryMaintainsTheProvider(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &maintainedProvider{}
	d := &daemon{provider: p, units: map[string]*unit{}}

	d.discover(context.Background())
	d.discover(context.Background())

	if p.calls != 2 {
		t.Fatalf("Maintain called %d times over two passes, want 2", p.calls)
	}
}
