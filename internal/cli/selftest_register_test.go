package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// slotProbe stops selftest at AllocSlot - the moment before its sandbox exists - and records
// which scoped daemons were registered right then.
type slotProbe struct {
	provider.Provider

	seen    []daemon.Presence
	removed bool
}

func (s *slotProbe) Name() string { return "fake" }

func (s *slotProbe) AllocSlot(context.Context, string) (int, error) {
	s.seen = daemon.Scoped()
	return 0, errors.New("stop here")
}

func (s *slotProbe) Remove(context.Context, string) error {
	s.removed = true
	return nil
}

// selftest's in-process daemon must be registered as a scoped daemon before its sandbox exists,
// and for no longer than selftest runs.
//
// It was not registered at all, so the machine's `sbx serve` - which leaves a live --only
// daemon's sandboxes to it - could not see it, adopted selftest-<pid> too, and logged "redis
// stopped serving :20040: bind: address already in use" on every run. Worse, it could win the
// port, and then selftest's own sleep-to-zero check was timing the wrong daemon. Registered
// before the create, the machine's daemon never adopts it; removed on every return, including a
// failure, the record never hides a later sandbox.
func TestSelftestRegistersItsExactScopeForItsLifetime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := &slotProbe{}
	name := fmt.Sprintf("selftest-%d", os.Getpid())

	if err := Selftest(context.Background(), p, provider.IsolationContainer, false); err == nil {
		t.Fatal("selftest passed against a provider that refuses a slot")
	}

	var mine *daemon.Presence

	for i := range p.seen {
		if p.seen[i].PID == os.Getpid() {
			mine = &p.seen[i]
		}
	}

	if mine == nil {
		t.Fatalf("selftest's daemon was not registered when its sandbox was about to be created (registry: %+v)", p.seen)
	}

	if !mine.Scope.Match(name) {
		t.Errorf("registered scope %v does not cover %s", mine.Scope, name)
	}

	if mine.Scope.Match(name + "1") {
		t.Errorf("registered scope %v also covers %s1 - another selftest's sandbox", mine.Scope, name)
	}

	for _, p := range daemon.Scoped() {
		if p.PID == os.Getpid() {
			t.Fatalf("selftest returned (on a failure) and left its registration behind: %+v", p)
		}
	}

	if !p.removed {
		t.Error("selftest's cleanup did not run")
	}
}
