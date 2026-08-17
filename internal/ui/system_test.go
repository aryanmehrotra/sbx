package ui

// The machine, not the fleet.
//
// "What is using the memory" is rarely answered by the sandboxes alone: a laptop's container
// runtime holds whatever else the day has left there, and a dashboard listing only its own
// services reports a nearly empty machine while the VM is full.

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/hostinfo"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/tui"
)

func machine() model {
	return model{
		version: "v0", metered: true,
		host: provider.Host{Cores: 4, MemBytes: 8 << 30, Name: "colima"},
		pane: paneSystem,
		neighbours: []provider.Neighbour{
			{Name: "sbx-work-mysql", Ours: true, Running: true, MemBytes: 356 << 20},
			{Name: "some-build-cache", Running: true, MemBytes: 2 << 30},
			{Name: "an-old-thing", Running: false},
		},
	}
}

func TestTheSystemPaneCountsWhatIsNotOurs(t *testing.T) {
	frame := plain(render(machine(), 26, 132))

	// The neighbour is the biggest thing on the machine and the reason it is full.
	if !strings.Contains(frame, "some-build-cache") {
		t.Errorf("a container sbx did not create is missing:\n%s", frame)
	}

	// The summary has to separate ours from everything else, or "2.3g used" tells somebody
	// their sandboxes are the problem when they are not.
	for _, want := range []string{"colima", "sbx", "other", "containers hold"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the summary never mentions %q:\n%s", want, frame)
		}
	}

	// A stopped container holds nothing, which is the point of the project - counted, not listed.
	if !strings.Contains(frame, "asleep, holding nothing") {
		t.Errorf("stopped containers are not accounted for:\n%s", frame)
	}
}

// It costs a round trip per running container, so it is sampled only while somebody is reading
// it - and `a` is what says they are.
func TestAOpensAndClosesTheSystemPane(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.model.rows = twoSandboxes()

	d.handle(context.Background(), tui.Key{Rune: 'a', Code: tui.KeyRune})

	if m, _ := d.snapshot(); m.pane != paneSystem {
		t.Fatalf("a did not open the system pane (pane = %v)", m.pane)
	}

	d.handle(context.Background(), tui.Key{Rune: 'a', Code: tui.KeyRune})

	if m, _ := d.snapshot(); m.pane == paneSystem {
		t.Error("a did not close it again")
	}
}

// Before the first sample lands there is nothing to draw, and an empty pane reads as a machine
// with nothing on it.
func TestTheSystemPaneSaysItIsStillReading(t *testing.T) {
	m := machine()
	m.neighbours = nil

	if frame := plain(render(m, 26, 132)); !strings.Contains(frame, "reading the machine") {
		t.Errorf("an unsampled machine looks like an empty one:\n%s", frame)
	}
}

// Two machines, and on macOS they are genuinely two computers. Their figures are not
// comparable and neither substitutes for the other: the VM is what the sandboxes contend for,
// the laptop is what the person is deciding about.
func TestBothMachinesAreReported(t *testing.T) {
	m := machine()
	m.machine = hostinfo.Machine{Cores: 10, MemBytes: 16 << 30, FreeBytes: 4500 << 20}

	frame := plain(render(m, 26, 132))

	if !strings.Contains(frame, "this machine") || !strings.Contains(frame, "10 cores") {
		t.Errorf("the computer the person is at is missing:\n%s", frame)
	}

	if !strings.Contains(frame, "colima") || !strings.Contains(frame, "4 cores") {
		t.Errorf("the runtime's own machine is missing:\n%s", frame)
	}

	// The laptop's kernel knows what is free; docker cannot say what else is inside the VM, so
	// the runtime line claims only what the containers hold.
	if !strings.Contains(frame, "free of") {
		t.Errorf("the laptop does not report free memory:\n%s", frame)
	}

	if !strings.Contains(frame, "containers hold") {
		t.Errorf("the runtime line overclaims - it can only speak for the containers:\n%s", frame)
	}
}

// The title carries both too, and unlabelled they read as one machine described twice.
func TestTheTitleSaysWhichMachineEachFigureIsAbout(t *testing.T) {
	m := machine()
	m.machine = hostinfo.Machine{Cores: 10, MemBytes: 16 << 30, FreeBytes: 4500 << 20}
	m.rows = []row{{Sandbox: "work", Service: "mysql", Awake: true, Ref: "r",
		MemBytes: 356 << 20, MemKnown: true}}

	title := strings.Split(plain(render(m, 26, 150)), "\n")[0]

	for _, want := range []string{"sbx ", "host "} {
		if !strings.Contains(title, want) {
			t.Errorf("the title does not say which machine %q belongs to:\n%s", want, title)
		}
	}
}
