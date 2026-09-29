package provider

import (
	"testing"
	"time"
)

// A VMM that is alive but did not answer its API in time is a VM that could not be asked, not
// one with nothing to check. Probe returned declared=false for it, which the CLI's wait reads as
// "serving" and the daemon's wake as "awake, unverified" - the false "serving" docker had when its
// engine stalled. Asleep (no process) is still nothing to ask, as Create relies on.
func TestFirecrackerProbeOfAnUnansweringVMMIsNotUndeclared(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "pstall", redis)

	if serving, declared := r.p.Probe(r.ctx, ref); serving || declared {
		t.Fatalf("Probe of an asleep VM = (%v, %v), want (false, false): nobody woke it", serving, declared)
	}

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	r.l.server(r.p.dir(ref)).StallNext("/", describeTimeout+300*time.Millisecond)

	if serving, declared := r.p.Probe(r.ctx, ref); serving || !declared {
		t.Errorf("Probe of a live VMM that did not answer = (%v, %v), want (false, true)", serving, declared)
	}
}
