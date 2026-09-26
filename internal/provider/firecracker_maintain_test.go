package provider

import (
	"context"
	"slices"
	"sync"
	"testing"
)

// guardNet is fakeNet that also records the daemon's guard re-checks.
type guardNet struct {
	*fakeNet
	mu      sync.Mutex
	checked []int
}

func (n *guardNet) EnsureGuard(_ context.Context, slot int) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.checked = append(n.checked, slot)
}

// The daemon's reconcile re-checks the host rules of every bridge a VM is on, once per bridge -
// not only when the bridge is made, which left a flushed rule gone until the sandbox was recreated.
func TestMaintainRechecksEveryBridgeOnce(t *testing.T) {
	r := newRig(t)
	g := &guardNet{fakeNet: r.n}
	r.p.net = g

	r.create(t, "a", redis)
	r.create(t, "b", redis)

	var m Maintainer = r.p
	m.Maintain(r.ctx)

	slots := slices.Clone(g.checked)
	slices.Sort(slots)

	if len(slots) != 2 || slots[0] == slots[1] {
		t.Fatalf("checked slots %v, want each sandbox's bridge once", g.checked)
	}
}
