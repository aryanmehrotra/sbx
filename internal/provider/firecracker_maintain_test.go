package provider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// Prewarm on firecracker builds the image's root filesystem, once; a create after it builds
// nothing, and a second prewarm says there was nothing to do.
func TestWarmBuildsTheRootfsOnceAndACreateReusesIt(t *testing.T) {
	r := newRig(t)

	var w Warmer = r.p

	for i, want := range []bool{true, false} {
		built, err := w.Warm(r.ctx, redis.Image)
		if err != nil || built != want {
			t.Fatalf("Warm #%d = %v, %v; want %v", i+1, built, err, want)
		}
	}

	if !r.p.rootfs.Cached(r.ctx, redis.Image) {
		t.Fatal("the rootfs Warm built is not in the cache a create reads")
	}
}

// wholeNet is fakeNet whose bridges' guards are as the test says.
type wholeNet struct {
	*fakeNet
	whole bool
}

func (n *wholeNet) GuardWhole(context.Context, int) (bool, error) { return n.whole, nil }

// The provider says, per sandbox, when its bridge's guard is not in place - so the API can tell
// the caller - and says nothing when it is.
func TestHostWarningsNameAnUnguardedBridge(t *testing.T) {
	r := newRig(t)
	n := &wholeNet{fakeNet: r.n, whole: true}
	r.p.net = n

	r.create(t, "hw", redis)

	var hw HostWarner = r.p
	if w := hw.HostWarnings(r.ctx, "hw"); len(w) != 0 {
		t.Fatalf("a guarded bridge warned: %q", w)
	}

	n.whole = false
	if w := hw.HostWarnings(r.ctx, "hw"); len(w) != 1 || !strings.Contains(w[0], "10.231.") || !strings.Contains(w[0], "SECURITY.md") {
		t.Fatalf("an unguarded bridge = %q", w)
	}

	if w := hw.HostWarnings(r.ctx, "nosuch"); len(w) != 0 {
		t.Fatalf("a sandbox that is not here warned: %q", w)
	}

	r.p.guardCheck = func() error { return errors.New("iptables is not on PATH") }
	if w := hw.HostWarnings(r.ctx, "hw"); len(w) != 1 || !strings.Contains(w[0], "iptables is not on PATH") {
		t.Fatalf("no iptables = %q", w)
	}
}
