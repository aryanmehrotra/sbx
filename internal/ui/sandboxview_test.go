package ui

// The sandbox view.
//
// The table is a list of services, and a sandbox with four of them is four lines repeating one
// name with no line for the thing itself - while every command in this program (`sbx env`,
// `sbx rm`, `sbx logs`) names a sandbox. `v` folds the services up.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/tui"
)

func twoSandboxes() []row {
	return []row{
		{Sandbox: "work", Service: "mysql", Awake: true, Ref: "r1", Address: "127.0.0.1:1",
			CPU: 5, CPUKnown: true, MemBytes: 300 << 20, MemKnown: true},
		{Sandbox: "work", Service: "redis", Awake: true, Ref: "r2", Address: "127.0.0.1:2",
			CPU: 2, CPUKnown: true, MemBytes: 4 << 20, MemKnown: true},
		{Sandbox: "obs", Service: "console", Ref: "r3", Address: "127.0.0.1:3"},
	}
}

func TestGroupingFoldsServicesIntoTheirSandbox(t *testing.T) {
	m := model{rows: twoSandboxes(), grouped: true}

	got := m.view()
	if len(got) != 2 {
		t.Fatalf("three services in two sandboxes folded to %d rows", len(got))
	}

	work := got[0]

	if work.Sandbox != "work" || work.Members != 2 || work.Woken != 2 {
		t.Errorf("work = %+v, want 2 services with both awake", work)
	}

	// A total, because the question a sandbox row answers is what the whole thing is costing.
	if work.CPU != 7 || work.MemBytes != (304<<20) {
		t.Errorf("work totals cpu=%v mem=%v, want 7 and 304 MiB", work.CPU, work.MemBytes)
	}

	// One awake service means the sandbox is costing memory, which is what the colour is for.
	obs := got[1]
	if obs.Woken != 0 || obs.Awake {
		t.Errorf("obs = %+v, want nothing awake", obs)
	}
}

// A sandbox is not simply awake or asleep, and a row standing for four things saying "AWAKE"
// because one of them is would be a fair summary of the cost and a bad one of the state.
func TestASandboxRowSaysHowManyAreUp(t *testing.T) {
	rows := twoSandboxes()
	rows[1].Awake = false // work now has one of two up

	frame := plain(render(model{version: "v0", rows: rows, grouped: true, metered: true}, 20, 120))

	if !strings.Contains(frame, "1/2 up") {
		t.Errorf("a half-woken sandbox does not say so:\n%s", frame)
	}

	if !strings.Contains(frame, "0/1 up") {
		t.Errorf("a sleeping sandbox does not say so:\n%s", frame)
	}
}

// The row is a total, and a total is the one thing that cannot say which service is the
// expensive one - so the block underneath is the list it folded up.
func TestTheDetailBlockListsWhatTheSandboxHolds(t *testing.T) {
	frame := plain(render(model{version: "v0", rows: twoSandboxes(), grouped: true, metered: true}, 20, 120))

	for _, want := range []string{"mysql", "redis", "127.0.0.1:1", "127.0.0.1:2", "sbx env work"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the sandbox detail never mentions %q:\n%s", want, frame)
		}
	}
}

// A key pressed on a line standing for two services has to act on two services. Waking nothing
// while appearing to wake a sandbox is the failure that would make this view a lie.
func TestAKeyOnASandboxActsOnEveryServiceInIt(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.model.rows = twoSandboxes()
	d.model.grouped = true
	d.model.selected = 0

	got := d.targets()
	if len(got) != 2 {
		t.Fatalf("a sandbox of two services targeted %d", len(got))
	}

	for i, want := range []string{"mysql", "redis"} {
		if got[i].Service != want {
			t.Errorf("target %d = %q, want %q", i, got[i].Service, want)
		}
	}

	// Ungrouped, the same key is about the one service it is pointing at.
	d.model.grouped = false
	d.model.selected = 1

	if got := d.targets(); len(got) != 1 || got[0].Service != "redis" {
		t.Errorf("a service row targeted %+v, want redis alone", got)
	}
}

// Logs and limits belong to one container, and a sandbox row stands for several. Saying so
// beats opening an empty pane or applying a ceiling to a name that is not a container.
//
// It is also where a deadlock lived: handle() holds the model's lock, and say() takes it, so
// the first version of this hung the dashboard on a keypress. The matrix test finds that as a
// five-minute timeout rather than a failure, which is a bad way to hear about it.
func TestLogsAndLimitSayTheyArePerService(t *testing.T) {
	for _, k := range []rune{'l', 'L'} {
		d := newDash(&fakeProvider{})
		d.model.rows = twoSandboxes()
		d.model.grouped = true

		done := make(chan struct{})

		go func() {
			d.handle(context.Background(), tui.Key{Rune: k, Code: tui.KeyRune})
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%c on a sandbox row never returned - the handler is holding a lock it "+
				"is also waiting for", k)
		}

		m, _ := d.snapshot()

		if !strings.Contains(m.message, "per service") {
			t.Errorf("%c said %q, want it to explain that this acts on one service", k, m.message)
		}

		if m.pane == paneLogs {
			t.Errorf("%c opened the log pane on a row that has no single service", k)
		}

		if m.input.active {
			t.Errorf("%c opened a limit prompt for a sandbox", k)
		}
	}
}

// Twelve services and six sandboxes are different numbers of rows, and the selection counts
// rows. Toggling with the last one selected must not leave it pointing past the end.
func TestTogglingKeepsTheSelectionInside(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.model.rows = twoSandboxes()
	d.model.selected = 2 // the third service; there are only two sandboxes

	d.handle(context.Background(), tui.Key{Rune: 'v', Code: tui.KeyRune})

	m, _ := d.snapshot()

	if !m.grouped {
		t.Fatal("v did not group")
	}

	if m.selected >= len(m.view()) {
		t.Errorf("selection %d is past the end of %d sandboxes", m.selected, len(m.view()))
	}

	// And back, without leaving it somewhere impossible either.
	d.handle(context.Background(), tui.Key{Rune: 'v', Code: tui.KeyRune})

	m, _ = d.snapshot()

	if m.grouped {
		t.Fatal("v did not ungroup")
	}

	if m.selected >= len(m.view()) {
		t.Errorf("selection %d is past the end of %d services", m.selected, len(m.view()))
	}
}
