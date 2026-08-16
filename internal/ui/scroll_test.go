package ui

import (
	"context"
	"strings"
	"testing"
	"time"

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

// The trap that shipped: pressing l to look at a log took the arrow keys away from the table,
// so moving to the next service did nothing and the dashboard felt frozen.
func TestOpeningLogsLeavesTheArrowsOnTheTable(t *testing.T) {
	d := newDash(&fakeProvider{})

	d.handle(context.Background(), tui.Key{Rune: 'l', Code: tui.KeyRune})

	if d.model.focus != focusTable {
		t.Fatal("opening the log pane took the arrows away from the table")
	}

	before := d.model.selected

	d.handle(context.Background(), tui.Key{Code: tui.KeyDown})

	if d.model.selected == before {
		t.Error("with logs open, down did not move to the next service")
	}
}

// Tab is the explicit way in, and it refuses to strand the arrows somewhere they do nothing.
func TestTabRefusesAFocusThatWouldDoNothing(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.paneHeight = 20
	d.model.pane = paneLogs
	d.model.logs = []string{"one", "two"} // fits with room to spare

	d.handle(context.Background(), tui.Key{Code: tui.KeyTab})

	if d.model.focus != focusTable {
		t.Error("tab moved the arrows to a pane with nothing to scroll, where they do nothing")
	}

	if d.message() == "" {
		t.Error("it moved nothing and said nothing, which is indistinguishable from a freeze")
	}
}

func TestTabMovesFocusWhenThereIsSomethingToScroll(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.paneHeight = 3
	d.model.pane = paneLogs
	d.model.logs = []string{"1", "2", "3", "4", "5", "6", "7", "8"}

	d.handle(context.Background(), tui.Key{Code: tui.KeyTab})

	if d.model.focus != focusPane {
		t.Fatal("tab did not move the arrows to a scrollable pane")
	}

	if got := stripColour(footer(d.model, 3, 120)); !strings.Contains(got, "scroll") {
		t.Errorf("the footer does not show the scroll keys: %q", got)
	}

	// And back again.
	d.handle(context.Background(), tui.Key{Code: tui.KeyTab})

	if d.model.focus != focusTable {
		t.Error("tab did not bring the arrows back to the table")
	}
}

// Whatever state the reader is in, one key returns them to the table.
func TestEscapeAlwaysReturnsToTheTable(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.model.focus = focusPane

	d.handle(context.Background(), tui.Key{Code: tui.KeyEscape})

	if d.model.focus != focusTable {
		t.Error("escape did not return the arrows to the table")
	}
}

// A footer must not advertise a key that does nothing right now.
func TestTheFooterOnlyNamesLiveKeys(t *testing.T) {
	m := model{version: "v", focus: focusPane, pane: paneEvents}
	m.events = nil

	got := stripColour(footer(m, 10, 120))

	for _, dead := range []string{"g top", "G follow", "⇞⇟ page"} {
		if strings.Contains(got, dead) {
			t.Errorf("the footer offers %q with nothing to scroll: %q", dead, got)
		}
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

	d.handle(context.Background(), tui.Key{Code: tui.KeyUp})
	d.handle(context.Background(), tui.Key{Code: tui.KeyUp})

	if d.model.selected != before {
		t.Errorf("scrolling the pane moved the table selection from %d to %d",
			before, d.model.selected)
	}

	if d.model.offset == 0 {
		t.Error("the pane did not scroll at all")
	}
}

// A reader who has scrolled back is reading something. Refetching under them would move the
// text, or silently change what "forty lines back" refers to.
func TestFollowingStopsWhenScrolledBack(t *testing.T) {
	p := &fakeProvider{}
	d := newDash(p)
	d.model.pane = paneLogs
	d.model.logs = []string{"stale"}

	// Following: the poll refills the pane.
	d.model.offset = 0
	d.followLogs(context.Background())

	if got := strings.Join(d.model.logs, " "); !strings.Contains(got, "line one") {
		t.Errorf("while following, the pane was not refreshed: %v", d.model.logs)
	}

	// Scrolled back: it is left alone.
	d.model.offset = 5
	d.model.logs = []string{"what the reader is looking at"}

	d.followLogs(context.Background())

	if got := strings.Join(d.model.logs, " "); !strings.Contains(got, "what the reader") {
		t.Errorf("a scrolled-back pane was refetched under the reader: %v", d.model.logs)
	}
}

// Nothing is fetched for a pane that is not open.
func TestNoLogReadsWhileThePaneIsClosed(t *testing.T) {
	p := &fakeProvider{}
	d := newDash(p)
	d.model.pane = paneEvents
	d.model.logs = nil

	d.followLogs(context.Background())

	if d.model.logs != nil {
		t.Error("logs were fetched with the pane showing events, which is a round trip per " +
			"service per second to answer a question nobody asked")
	}
}

// Feedback from a keypress must not become permanent furniture: one `s` used to leave its
// message in the footer for the rest of the session, and the key hints never came back.
func TestAMessageFadesAndTheHintsReturn(t *testing.T) {
	m := model{version: "v", message: "zn-dev/mysql asleep", messageAt: time.Now()}

	if got := stripColour(footer(m, 5, 120)); !strings.Contains(got, "asleep") {
		t.Errorf("a fresh message is not shown: %q", got)
	}

	m.messageAt = time.Now().Add(-2 * messageLife)

	got := stripColour(footer(m, 5, 120))

	if strings.Contains(got, "asleep") {
		t.Errorf("an old message is still in the footer: %q", got)
	}

	if !strings.Contains(got, "quit") {
		t.Errorf("the key hints did not come back: %q", got)
	}
}
