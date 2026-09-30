package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/egress"
)

// The CLI and the daemon are two processes, each with its own EgressControl over one directory
// and one filter. These tests stand them up as two instances and run one inside the other's
// write, at the point where the interleaving did its damage.

// twoWriters is the CLI's EgressControl and the daemon's, over the same filter and directory, with
// a saved live policy that denies files.pypi.org - so the daemon's Sync has something to push.
func twoWriters(t *testing.T) (cli, daemon *EgressControl, f *egress.Filter) {
	t.Helper()

	cli, f, fp := newContainerSetup(t)
	daemon = NewEgressControl(fp, cli.dir)

	if _, err := cli.PatchPolicy(context.Background(), "osb-1", "",
		[]egress.Rule{{Action: "deny", Target: "files.pypi.org"}}); err != nil {
		t.Fatal(err)
	}

	if f.Permits("files.pypi.org") {
		t.Fatal("setup: the deny did not reach the filter")
	}

	return cli, daemon, f
}

// during runs fn on its own goroutine and waits for it for up to a second, the time an
// unserialised fn needs many times over. A serialised one is still blocked then, and is waited
// for by the returned func instead.
func during(t *testing.T, fn func() error) (wait func()) {
	t.Helper()

	done := make(chan error, 1)

	go func() { done <- fn() }()

	var (
		err      error
		finished bool
	)

	select {
	case err = <-done:
		finished = true
	case <-time.After(time.Second):
	}

	return func() {
		t.Helper()

		if !finished {
			err = <-done
		}

		if err != nil {
			t.Errorf("the interleaved call failed: %v", err)
		}
	}
}

// The CLI removes a rule; the daemon's Sync runs between the CLI's push and its save, when the
// saved copy still has the rule and the filter no longer does. Unserialised, Sync puts the
// removed rule back, and the filter enforces it until the next tick.
func TestSyncBetweenACLIPushAndItsSaveDoesNotRevertTheWrite(t *testing.T) {
	cli, daemon, f := twoWriters(t)
	ctx := context.Background()

	// Sync must outwait the CLI's write, which is held here for during's one second. At the
	// production second the two timeouts tie, and under load Sync defers to the next tick - correct,
	// but then this test never sees Sync run after the write, which is what it is here to check.
	defer func(w time.Duration) { syncLockWait = w }(syncLockWait)
	syncLockWait = 10 * time.Second

	var wait func()

	cli.hook = func(point string) {
		if point == "pushed" && wait == nil {
			wait = during(t, func() error { return daemon.Sync(ctx, "osb-1") })
		}
	}

	if _, err := cli.DeleteRules(ctx, "osb-1", "", []string{"files.pypi.org"}); err != nil {
		t.Fatal(err)
	}

	if wait == nil {
		t.Fatal("the hook never ran: the test no longer reaches the point between push and save")
	}

	wait()

	if !f.Permits("files.pypi.org") {
		t.Fatalf("the filter enforces the policy the CLI just replaced: %+v", f.Policy())
	}

	saved, _ := cli.load("osb-1")
	if saved.Policy.Hash() != f.Policy().Hash() {
		t.Fatalf("the saved policy and the one in force differ:\nsaved %+v\nlive  %+v", saved.Policy, f.Policy())
	}
}

// The other half: Sync loads the saved copy, then the CLI pushes and saves, then Sync reads what
// is in force - the CLI's, with a fresh tag - and pushes the copy it loaded before. Reordering the
// CLI's push and save does not close this; only holding one lock across Sync's load and push does.
func TestACLIWriteBetweenSyncsLoadAndPushIsNotReverted(t *testing.T) {
	cli, daemon, f := twoWriters(t)
	ctx := context.Background()

	var wait func()

	daemon.hook = func(point string) {
		if point == "sync-loaded" && wait == nil {
			wait = during(t, func() error {
				_, err := cli.DeleteRules(ctx, "osb-1", "", []string{"files.pypi.org"})
				return err
			})
		}
	}

	// A different policy in force than the one saved, so Sync has a reason to push.
	if err := f.SetPolicy(declared()); err != nil {
		t.Fatal(err)
	}

	if err := daemon.Sync(ctx, "osb-1"); err != nil {
		t.Fatal(err)
	}

	if wait == nil {
		t.Fatal("the hook never ran: Sync no longer reaches the point after its load")
	}

	wait()

	if !f.Permits("files.pypi.org") {
		t.Fatalf("the filter enforces the policy the CLI just replaced: %+v", f.Policy())
	}

	saved, _ := cli.load("osb-1")
	if saved.Policy.Hash() != f.Policy().Hash() {
		t.Fatalf("the saved policy and the one in force differ:\nsaved %+v\nlive  %+v", saved.Policy, f.Policy())
	}
}
