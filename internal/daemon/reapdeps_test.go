package daemon

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

// A dependency in use must survive the reaper.
//
// The traffic that proves a dependency is busy never reaches the daemon: the dependent dials
// it over the sandbox's own network, so the dependency's idle clock expires while it is under
// constant load. Sleeping it takes its name out of the network's DNS and the dependent dies on
// `no such host`.
//
// Measured on a live two-service sandbox before this: a continuously-served `app` lost four
// dials to `db` every fifteen seconds, indefinitely, while `sbx list` still called db awake.
func TestReaperKeepsADependencyAnAwakeServiceNeeds(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingProvider{}
	p.serving.Store(true)

	d := New(p, time.Minute, time.Minute, time.Minute)

	db := newUnit("s", "db", "sbx-s-db", "inst-db", "s/db", nil, true)
	app := newUnit("s", "app", "sbx-s-app", "inst-app", "s/app", nil, true)

	app.dependsOn = []string{"db"}

	// Both look idle to the daemon. That is the whole point: app's dials to db are invisible
	// to it, and here even app's own clock has run out.
	for _, u := range []*unit{db, app} {
		u.served = true
		u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())
	}

	// app is awake and needs db; only app is eligible to sleep this tick.
	d.units["sbx-s-db"] = db
	d.units["sbx-s-app"] = app

	d.reap(context.Background())

	if db.isAwake() != true {
		t.Error("db was slept while an awake app depended on it - the dependent's next dial " +
			"gets `no such host`, which is the failure dependency-ordered wake exists to prevent")
	}

	if app.isAwake() {
		t.Error("app was idle and nothing depends on it; it should have been slept")
	}

	// With app asleep, db is no longer needed and the next tick may take it. A stack sleeps
	// from the top down rather than all at once, and that ordering is the point.
	d.reap(context.Background())

	if db.isAwake() {
		t.Error("db stayed awake after its only dependent slept, so the stack never reaches 0 B")
	}
}

// Declaring nothing must cost nothing: a unit with no dependents sleeps on its own clock.
func TestReaperStillSleepsAnUnneededService(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingProvider{}
	p.serving.Store(true)

	d := New(p, time.Minute, time.Minute, time.Minute)

	u := newUnit("s", "lonely", "sbx-s-lonely", "inst", "s/lonely", nil, true)
	u.served = true
	u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

	d.units["sbx-s-lonely"] = u

	d.reap(context.Background())

	if u.isAwake() {
		t.Error("an idle service nothing depends on was not slept")
	}
}

// Names resolve within one sandbox. A service called db in another sandbox must not be kept
// awake by this one's app, or one stack pins another's datastore forever.
func TestReaperDoesNotHoldAnotherSandboxsService(t *testing.T) {
	log.SetOutput(io.Discard)

	p := &countingProvider{}
	p.serving.Store(true)

	d := New(p, time.Minute, time.Minute, time.Minute)

	mine := newUnit("a", "app", "sbx-a-app", "inst-a", "a/app", nil, true)
	mine.dependsOn = []string{"db"}

	theirs := newUnit("b", "db", "sbx-b-db", "inst-b", "b/db", nil, true)

	for _, u := range []*unit{mine, theirs} {
		u.served = true
		u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())
	}

	d.units["sbx-a-app"] = mine
	d.units["sbx-b-db"] = theirs

	d.reap(context.Background())

	if theirs.isAwake() {
		t.Error("sandbox a's app kept sandbox b's db awake; depends_on names resolve within " +
			"one sandbox only")
	}
}
