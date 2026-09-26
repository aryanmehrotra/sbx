package daemon

import (
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A daemon started with --only fronts, wakes and sleeps every sandbox in its scope, so every
// CLI verb that asks "is a daemon serving this sandbox?" must find it - and only for sandboxes
// inside that scope. It must still not claim the machine's one-per-machine record: a scoped
// daemon beside the machine's own is the documented shape, and `sbx serve` would refuse it.
func TestAScopedDaemonIsFoundForTheSandboxesItServes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	scope, err := ParseScope([]string{"osb-,ci-*"})
	if err != nil {
		t.Fatal(err)
	}

	clear := Announce("docker", scope)

	for _, sb := range []string{"osb-a", "ci-7"} {
		p, ok := Serving(sb)
		if !ok {
			t.Fatalf("Serving(%q) found no daemon, but one scoped to %s is running", sb, scope)
		}

		if !scope.Match(sb) || len(p.Scope) == 0 {
			t.Errorf("Serving(%q) = %+v, want the scoped daemon's record with its scope", sb, p)
		}
	}

	if _, ok := Serving("zopnight"); ok {
		t.Error("Serving found a daemon for a sandbox outside every running daemon's --only")
	}

	if _, ok := Running(); ok {
		t.Error("a scoped daemon claimed the machine daemon's record; `sbx serve` would now refuse to start")
	}

	clear()

	if _, ok := Serving("osb-a"); ok {
		t.Error("the scoped daemon's record outlived it")
	}
}

// The unscoped daemon serves everything, scoped records or not.
func TestTheMachineDaemonServesEverySandbox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	clear := Announce("docker", nil)
	defer clear()

	if _, ok := Serving("anything"); !ok {
		t.Error("the unscoped daemon was not found for a sandbox")
	}
}

// An egress policy saved for a sandbox a scoped daemon hosts is applied by that daemon, and the
// note beside it must say so rather than "sbx serve is not running".
func TestEgressNoteFindsAScopedDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	scope, _ := ParseScope([]string{"osb-"})
	defer Announce("docker", scope)()

	c := &EgressControl{}

	if got := c.note(provider.EgressFilter{Sandbox: "osb-a"}); strings.Contains(got, "not running") {
		t.Errorf("note for a sandbox inside a running daemon's --only = %q", got)
	}

	if got := c.note(provider.EgressFilter{Sandbox: "zopnight"}); !strings.Contains(got, "not running") {
		t.Errorf("note for a sandbox no daemon hosts = %q, want it to say none is running", got)
	}
}
