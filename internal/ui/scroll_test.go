package ui

import (
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/tui"
)

func logsModel(n int) model {
	m := model{version: "v", pane: paneLogs, focus: focusPane}

	for i := range n {
		m.logs = append(m.logs, "line "+string(rune('A'+i%26))+strings.Repeat("", 0))
	}

	// Distinct, findable content.
	m.logs = nil
	for i := range n {
		m.logs = append(m.logs, "entry-"+itoa(i))
	}

	m.rows = []row{{Sandbox: "s", Service: "svc", Ref: "r"}}

	return m
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}

	var b []byte

	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}

	return string(b)
}

// Offset 0 is the tail. A log that opens anywhere but the end is one you have to scroll before
// it tells you anything.
func TestThePaneOpensAtTheEnd(t *testing.T) {
	m := logsModel(200)

	got := stripColour(strings.Join(paneBody(m, 5, 100), "\n"))

	if !strings.Contains(got, "entry-199") {
		t.Errorf("the pane does not show the newest line:\n%s", got)
	}

	if strings.Contains(got, "entry-0") {
		t.Errorf("the pane opened at the beginning:\n%s", got)
	}
}

func TestScrollingBackAndReturningToTheTail(t *testing.T) {
	m := logsModel(200)

	m.offset = 50

	got := stripColour(strings.Join(paneBody(m, 5, 100), "\n"))

	if !strings.Contains(got, "entry-149") {
		t.Errorf("scrolling back 50 did not move the window:\n%s", got)
	}

	if strings.Contains(got, "entry-199") {
		t.Errorf("the window still shows the tail after scrolling back:\n%s", got)
	}
}

// The window never runs off either end: past the top there is nothing to show, and past the
// bottom is the tail.
func TestTheWindowStaysInsideTheContent(t *testing.T) {
	m := logsModel(20)

	for _, off := range []int{0, 5, 15, 19, 20, 500} {
		m.offset = off

		lines := paneBody(m, 6, 100)

		if len(lines) != 6 {
			t.Fatalf("offset %d produced %d lines, want 6", off, len(lines))
		}

		body := stripColour(strings.Join(lines, "\n"))

		// Whatever the offset, something real is on screen: a pane scrolled into the void is
		// how somebody concludes the logs are gone.
		if !strings.Contains(body, "entry-") {
			t.Errorf("offset %d showed nothing at all:\n%s", off, body)
		}
	}
}

// The keys have to be bounded by the pane's height, or holding one walks the window into
// empty space and the reader has to guess how far back to come.
func TestScrollKeysAreBounded(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.model = logsModel(30)
	d.paneHeight = 10

	for range 100 {
		d.scrollPane(tui.Key{Code: tui.KeyUp})
	}

	if want := 30 - 10; d.model.offset != want {
		t.Errorf("scrolling up 100 times left offset at %d, want %d (the first line)",
			d.model.offset, want)
	}

	for range 100 {
		d.scrollPane(tui.Key{Code: tui.KeyDown})
	}

	if d.model.offset != 0 {
		t.Errorf("scrolling down 100 times left offset at %d, want 0 (following)", d.model.offset)
	}
}

func TestGoToTopAndFollow(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.model = logsModel(40)
	d.paneHeight = 8

	d.scrollPane(tui.Key{Rune: 'g'})

	if d.model.offset != 32 {
		t.Errorf("g left offset at %d, want 32 (the top)", d.model.offset)
	}

	d.scrollPane(tui.Key{Rune: 'G'})

	if d.model.offset != 0 {
		t.Errorf("G left offset at %d, want 0 (following the tail)", d.model.offset)
	}
}

// Tab moves the arrows between the two halves, and the footer has to say which one they are
// driving - otherwise down means two different things and nothing on screen says which.
func TestTabMovesFocusAndTheFooterFollows(t *testing.T) {
	d := newDash(&fakeProvider{})

	if d.model.focus != focusTable {
		t.Fatal("focus does not start on the table")
	}

	d.handle(nil, tui.Key{Code: tui.KeyTab})

	if d.model.focus != focusPane {
		t.Fatal("tab did not move focus to the pane")
	}

	if got := stripColour(footer(d.model, 120)); !strings.Contains(got, "scroll") {
		t.Errorf("with the pane focused the footer still shows the table's keys: %q", got)
	}

	d.handle(nil, tui.Key{Code: tui.KeyTab})

	if got := stripColour(footer(d.model, 120)); !strings.Contains(got, "wake") {
		t.Errorf("back on the table the footer does not show its keys: %q", got)
	}
}

// With the pane focused, the arrows must not also be moving the table selection underneath.
func TestScrollingDoesNotMoveTheTableSelection(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.model.focus = focusPane
	d.model.pane = paneLogs
	d.model.logs = []string{"a", "b", "c", "d", "e", "f"}
	d.paneHeight = 2

	before := d.model.selected

	d.handle(nil, tui.Key{Code: tui.KeyUp})
	d.handle(nil, tui.Key{Code: tui.KeyUp})

	if d.model.selected != before {
		t.Errorf("scrolling the pane moved the table selection from %d to %d",
			before, d.model.selected)
	}

	if d.model.offset == 0 {
		t.Error("the pane did not scroll at all")
	}
}
