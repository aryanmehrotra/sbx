package provider

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
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

// A guest printing in a loop grows console.log for as long as the VM runs; the daemon's
// reconcile cuts it back, so the host's disk is not the guest's to fill.
func TestMaintainCapsAConsoleTheGuestKeepsWritingTo(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "loud", redis)

	path := filepath.Join(r.p.dir(ref), fc.ConsoleName)
	if err := os.WriteFile(path, bytes.Repeat([]byte("spam spam spam\n"), (fc.ConsoleMax/15)+1024), 0o600); err != nil {
		t.Fatal(err)
	}

	r.p.Maintain(r.ctx)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if st.Size() > fc.ConsoleKeep+4096 {
		t.Fatalf("console.log is %d bytes after reconcile, want at most about %d", st.Size(), fc.ConsoleKeep)
	}
}
