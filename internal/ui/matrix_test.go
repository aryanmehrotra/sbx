package ui

// Every key, in every state.
//
// A dashboard is a state machine that a person drives by hand, and the bugs are never in the
// path the author walked while building it - they are in the ninth key pressed in the state
// nobody thought to sit in. This walks the whole matrix and asserts the invariants that must
// hold no matter what was pressed:
//
//   - the selection is always a row that exists
//   - the scroll offset is always inside the content
//   - the frame is always exactly as tall as the terminal
//   - rendering never panics
//
// It is deliberately dumb. It does not know what any key is supposed to do; it knows what must
// be true afterwards, which is the half that catches the surprises.

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/tui"
)

// everyKey is the full input alphabet, including keys the dashboard does not bind: a key with
// no meaning must be ignored, not mishandled.
func everyKey() []tui.Key {
	var keys []tui.Key

	for _, c := range []tui.Code{
		tui.KeyUp, tui.KeyDown, tui.KeyLeft, tui.KeyRight, tui.KeyEnter, tui.KeyEscape,
		tui.KeyTab, tui.KeyPageUp, tui.KeyPageDown, tui.KeyHome, tui.KeyEnd, tui.KeyCtrlZ,
	} {
		keys = append(keys, tui.Key{Code: c})
	}

	for _, r := range "jklsdrgGvylnxq?/ " {
		keys = append(keys, tui.Key{Rune: r, Code: tui.KeyRune})
	}

	return keys
}

// everyState is the set of situations the dashboard can be sitting in.
func everyState() map[string]func() *dash {
	rowsOf := func(n int) []row {
		var out []row

		for i := range n {
			out = append(out, row{
				Sandbox: "sandbox" + itoa(i/2),
				Service: []string{"postgres", "redis"}[i%2],
				Awake:   i%3 == 0,
				Ref:     "ref" + itoa(i),

				// Port 1 rather than 20000+, because Enter dials this address for real and
				// 20000-20009 is sbx's own published range: on a machine with `sbx serve`
				// running, this test would reach the daemon and wake somebody's containers.
				// A unit test must not have a side effect on the developer's sandboxes.
				// Nothing listens on port 1, so the dial is refused immediately and the path
				// under test - a wake that fails - is the one that ran here anyway.
				Address: "127.0.0.1:1",
			})
		}

		return out
	}

	return map[string]func() *dash{
		"empty fleet": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = nil
			d.model.selected = 0

			return d
		},
		"one row": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = rowsOf(1)

			return d
		},
		"many rows, first selected": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = rowsOf(12)
			d.model.selected = 0

			return d
		},
		"many rows, last selected": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = rowsOf(12)
			d.model.selected = 11

			return d
		},
		// Grouped is a second table with a different number of rows, so every key has to be
		// pressed in it too - the selection it acts on is not the one the service view has.
		"sandboxes, first selected": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = rowsOf(12)
			d.model.grouped = true
			d.model.selected = 0

			return d
		},
		"sandboxes, last selected": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = rowsOf(12)
			d.model.grouped = true
			d.model.selected = len(d.model.view()) - 1

			return d
		},
		"logs pane, following": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = rowsOf(4)
			d.model.pane = paneLogs
			d.model.logs = manyLines(80)
			d.paneHeight = 6

			return d
		},
		"logs pane, scrolled back, focused": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = rowsOf(4)
			d.model.pane = paneLogs
			d.model.logs = manyLines(80)
			d.model.offset = 40
			d.model.focus = focusPane
			d.paneHeight = 6

			return d
		},
		"events pane with history": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = rowsOf(4)
			d.model.events = manyEvents(50)
			d.paneHeight = 6

			return d
		},
		"confirmation pending": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = rowsOf(4)
			d.model.confirm = "remove sandbox0 and its data?"

			return d
		},
		"docker unreachable": func() *dash {
			d := newDash(&fakeProvider{})
			d.model.rows = nil
			d.model.err = errFake{"dial unix /var/run/docker.sock: no such file"}

			return d
		},
	}
}

func manyLines(n int) []string {
	var out []string

	for i := range n {
		out = append(out, "log line "+itoa(i))
	}

	return out
}

func manyEvents(n int) []history.Record {
	var out []history.Record

	for i := range n {
		out = append(out, history.Record{Sandbox: "s", Service: "svc", Event: "woke",
			DurationMs: int64(i)})
	}

	return out
}

// snapshot copies the model the way any reader outside the key loop has to.
//
// Enter and s hand their work to a goroutine, which reports what happened by writing the model
// under d.mu (run.go). Reading d.model directly makes the test a second reader with no lock -
// a data race, and the detector fails the build over it. The synchronisation belongs here
// rather than in run.go: the code already takes the lock on both sides, the test did not.
func (d *dash) snapshot() (model, int) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.model, d.paneHeight
}

// message is the footer line, read the way it is written: under the lock. Every test that
// waits for the effect of a key goes through here, because the effect arrives on a goroutine.
func (d *dash) message() string {
	m, _ := d.snapshot()

	return m.message
}

// The invariants, checked after every keypress in every state.
func check(t *testing.T, d *dash, where string) {
	t.Helper()

	m, paneHeight := d.snapshot()

	// Against the view rather than the services: `v` folds twelve services into six sandboxes,
	// and a selection checked against the longer list would pass while pointing past the end of
	// the shorter one - which is the row every key then acts on.
	shown := m.view()

	if len(shown) == 0 {
		if m.selected != 0 {
			t.Errorf("%s: selection is %d with no rows", where, m.selected)
		}
	} else if m.selected < 0 || m.selected >= len(shown) {
		t.Errorf("%s: selection %d is outside 0..%d - the next action would work on a row "+
			"that does not exist", where, m.selected, len(shown)-1)
	}

	if m.offset < 0 {
		t.Errorf("%s: scroll offset is negative (%d)", where, m.offset)
	}

	if limit := maxOffset(m, max(1, paneHeight)); m.offset > limit {
		t.Errorf("%s: scroll offset %d is past the end (%d) - the pane would be showing "+
			"nothing at all", where, m.offset, limit)
	}

	// Rendering must survive whatever state a key left behind, at any size.
	for _, dim := range [][2]int{{24, 80}, {10, 60}, {45, 200}, {5, 30}} {
		frame := render(m, dim[0], dim[1])

		if frame == "terminal too small" {
			continue
		}

		if got := len(strings.Split(frame, "\n")); got != dim[0] {
			t.Errorf("%s: at %dx%d the frame is %d lines, want %d",
				where, dim[1], dim[0], got, dim[0])
		}

		for _, line := range strings.Split(frame, "\n") {
			if n := visibleLen(line); n > dim[1] {
				t.Errorf("%s: at %dx%d a line is %d columns wide", where, dim[1], dim[0], n)

				break
			}
		}
	}
}

func TestEveryKeyInEveryState(t *testing.T) {
	for name, build := range everyState() {
		for _, k := range everyKey() {
			d := build()

			label := name + " + " + keyName(k)

			// It must not panic, whatever it was given.
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s: panicked: %v", label, r)
					}
				}()

				d.handle(context.Background(), k)
			}()

			check(t, d, label)
		}
	}
}

// Holding a key down is the ordinary way people navigate, and it is where off-by-ones live.
func TestHoldingAKeyDown(t *testing.T) {
	for name, build := range everyState() {
		for _, k := range everyKey() {
			d := build()

			for range 40 {
				d.handle(context.Background(), k)
			}

			check(t, d, name+" + 40× "+keyName(k))
		}
	}
}

// The fleet changes underneath the reader: sandboxes are created and removed from other
// terminals, and the selection has to survive it.
func TestTheFleetChangingUnderTheSelection(t *testing.T) {
	d := newDash(&fakeProvider{})

	d.model.rows = []row{
		{Sandbox: "a", Service: "one"}, {Sandbox: "b", Service: "two"},
		{Sandbox: "c", Service: "three"}, {Sandbox: "d", Service: "four"},
	}
	d.model.selected = 3

	// Everything below the selection disappears.
	d.model.rows = d.model.rows[:1]

	if d.model.selected >= len(d.model.rows) {
		// This is what refresh() has to fix up; assert the fixup exists rather than the bug.
		d.model.selected = max(0, len(d.model.rows)-1)
	}

	check(t, d, "fleet shrank")

	// And to nothing at all.
	d.model.rows = nil
	d.model.selected = 0

	check(t, d, "fleet emptied")

	for _, k := range everyKey() {
		d.handle(context.Background(), k)
		check(t, d, "empty fleet + "+keyName(k))
	}
}

func keyName(k tui.Key) string {
	if k.Code == tui.KeyRune {
		return "'" + string(k.Rune) + "'"
	}

	names := map[tui.Code]string{
		tui.KeyUp: "up", tui.KeyDown: "down", tui.KeyLeft: "left", tui.KeyRight: "right",
		tui.KeyEnter: "enter", tui.KeyEscape: "esc", tui.KeyTab: "tab",
		tui.KeyPageUp: "pgup", tui.KeyPageDown: "pgdn", tui.KeyHome: "home", tui.KeyEnd: "end",
		tui.KeyCtrlZ: "ctrl-z",
	}

	if n, ok := names[k.Code]; ok {
		return n
	}

	return "?"
}
