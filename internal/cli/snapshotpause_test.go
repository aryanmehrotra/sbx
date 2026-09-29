package cli

// A snapshot of a running database copied its files while it wrote them: ClickHouse merging a
// part made `cp` fail on a file that vanished mid-copy, and a copy that did succeed was torn.
// Every running service is now paused for the copy and the commit, and always unpaused.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/snapshotpause"
)

func (e *engine) Pause(_ context.Context, ref string) error {
	e.events = append(e.events, "pause "+ref)
	return nil
}

func (e *engine) Unpause(_ context.Context, ref string) error {
	e.events = append(e.events, "unpause "+ref)
	return nil
}

// index of the first event equal to ev, or -1.
func at(events []string, ev string) int { return slices.Index(events, ev) }

func TestSnapshotPausesRunningServicesForTheCopyAndCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	e := newEngine(units("app", "db", "web")...)
	e.volumes["sbx-app-db-data"] = 3

	if _, err := Snapshot(context.Background(), e, "app", "gold"); err != nil {
		t.Fatal(err)
	}

	ev := e.events
	for _, svc := range []string{"db", "web"} {
		ref := "sbx-app-" + svc
		p, c, u := at(ev, "pause "+ref), at(ev, "commit "+ref), at(ev, "unpause "+ref)

		if p < 0 || c < 0 || u < 0 || !(p < c && c < u) {
			t.Errorf("%s: want pause < commit < unpause, events %v", svc, ev)
		}
	}

	if cp := at(ev, "copy sbx-app-db-data"); cp < at(ev, "pause sbx-app-db") || cp > at(ev, "unpause sbx-app-db") || cp < 0 {
		t.Errorf("db's volume was copied outside its pause: %v", ev)
	}

	for _, r := range []string{"sbx-app-db", "sbx-app-web"} {
		if snapshotpause.Held(r) {
			t.Errorf("%s is still marked paused after the snapshot", r)
		}
	}
}

// A failed copy still unpauses: a snapshot must never leave a database frozen.
func TestAFailedSnapshotStillUnpauses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	e := newEngine(units("app", "db", "cache")...)
	e.volumes["sbx-app-db-data"] = 3
	e.volumes["sbx-app-cache-data"] = 1
	e.copyErr["sbx-app-cache-data"] = errors.New("cp: can't stat '/from/./store/x': No such file or directory")

	if _, err := Snapshot(context.Background(), e, "app", "gold"); err == nil {
		t.Fatal("the failed copy reported success")
	}

	for _, r := range []string{"sbx-app-db", "sbx-app-cache"} {
		if at(e.events, "pause "+r) < 0 || at(e.events, "unpause "+r) < at(e.events, "pause "+r) {
			t.Errorf("%s was not paused and then unpaused: %v", r, e.events)
		}

		if snapshotpause.Held(r) {
			t.Errorf("%s is still marked paused after a failed snapshot", r)
		}
	}
}

// A service asleep is copied as it is. One frozen by on_idle stays frozen: snapshot did not
// pause it, so it must not thaw it.
func TestSnapshotLeavesStoppedAndFrozenServicesAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	us := units("app", "db", "cache")
	us[0].Running = false                     // asleep
	us[1].Running, us[1].Paused = false, true // frozen
	e := newEngine(us...)
	e.volumes["sbx-app-db-data"] = 3

	if _, err := Snapshot(context.Background(), e, "app", "gold"); err != nil {
		t.Fatal(err)
	}

	for _, ev := range e.events {
		if strings.HasPrefix(ev, "pause ") || strings.HasPrefix(ev, "unpause ") {
			t.Errorf("snapshot touched a service it did not find running: %v", e.events)
		}
	}
}

// While a service is paused for the snapshot the mark is there, so a daemon tick that sees the
// pause leaves it alone.
func TestTheMarkIsHeldForTheLengthOfThePause(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	e := &markEngine{engine: newEngine(units("app", "db")...)}
	e.volumes["sbx-app-db-data"] = 3

	if _, err := Snapshot(context.Background(), e, "app", "gold"); err != nil {
		t.Fatal(err)
	}

	if !e.heldAtCopy || !e.heldAtCommit {
		t.Errorf("mark during copy=%v commit=%v, want both", e.heldAtCopy, e.heldAtCommit)
	}
}

type markEngine struct {
	*engine
	heldAtCopy, heldAtCommit bool
}

func (m *markEngine) CopyVolume(ctx context.Context, src, dst string) error {
	m.heldAtCopy = snapshotpause.Held("sbx-app-db")
	return m.engine.CopyVolume(ctx, src, dst)
}

func (m *markEngine) Commit(ctx context.Context, ref, image string, changes ...string) error {
	m.heldAtCommit = snapshotpause.Held("sbx-app-db")
	return m.engine.Commit(ctx, ref, image, changes...)
}

var _ provider.Pauser = (*engine)(nil)
