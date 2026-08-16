package ui

import (
	"strings"
	"testing"
)

// The screen is always full. The first version pinned every pane at a fixed height and padded
// the middle with blank lines, so on a tall terminal most of the dashboard was nothing at all
// and it read as a program that had stopped.
func TestTheTableGrowsToFillTheScreen(t *testing.T) {
	// Enough rows that the table could always use more space than it is given.
	m := model{version: "v0.2.0"}
	for i := range 60 {
		m.rows = append(m.rows, row{Sandbox: "s", Service: string(rune('a' + i%26)), Address: "127.0.0.1:20000"})
	}

	prev := 0

	for _, rows := range []int{14, 20, 30, 45, 60} {
		l := plan(rows, len(m.rows), detailFull)

		if l.tableRows <= prev {
			t.Errorf("at %d rows the table got %d lines, no more than the %d it had on a "+
				"shorter terminal", rows, l.tableRows, prev)
		}

		prev = l.tableRows

		// And no more than a third of a tall screen may be the bottom pane.
		if l.paneRows > rows/3 {
			t.Errorf("at %d rows the pane took %d lines", rows, l.paneRows)
		}
	}
}

// With a handful of sandboxes on a tall terminal there is nothing to fill the table with, so
// the slack goes to the pane rather than becoming a void in the middle of the screen.
func TestSlackGoesToThePaneNotToNothing(t *testing.T) {
	l := plan(50, 3, detailFull)

	if l.paneRows < 5 {
		t.Errorf("with 3 sandboxes on a 50-row terminal the pane got only %d lines, leaving "+
			"the rest of the screen empty", l.paneRows)
	}
}

// Small terminals lose panes in order of what they are worth, and never lose the ability to
// read the table or to quit.
func TestSmallTerminalsDegradeInOrder(t *testing.T) {
	cases := []struct {
		rows                          int
		wantPane, wantDetail, wantFtr bool
	}{
		{40, true, true, true},
		{16, true, true, true},
		{12, false, true, true},  // the log pane is not worth its chrome here
		{8, false, false, true},  // detail goes, the keys stay
		{5, false, false, false}, // the table, and nothing else
	}

	for _, c := range cases {
		l := plan(c.rows, 5, detailFull)

		if (l.paneRows > 0) != c.wantPane {
			t.Errorf("at %d rows: pane=%d, wanted present=%v", c.rows, l.paneRows, c.wantPane)
		}

		if (l.detailRows > 0) != c.wantDetail {
			t.Errorf("at %d rows: detailRows=%d, want present=%v", c.rows, l.detailRows, c.wantDetail)
		}

		if l.footer != c.wantFtr {
			t.Errorf("at %d rows: footer=%v, want %v", c.rows, l.footer, c.wantFtr)
		}

		if l.tableRows < 1 {
			t.Errorf("at %d rows the table got no lines at all", c.rows)
		}
	}
}

// Whatever the terminal, the frame is exactly that many lines: shorter leaves the previous
// frame's tail on screen, taller scrolls the header away for good.
func TestTheFrameIsExactlyAsTallAsTheTerminal(t *testing.T) {
	for _, rows := range []int{4, 5, 8, 12, 16, 24, 40, 80} {
		for _, cols := range []int{24, 40, 60, 80, 120, 200} {
			for _, m := range []model{{version: "v"}, sample()} {
				frame := render(m, rows, cols)

				if frame == "terminal too small" {
					continue
				}

				if got := len(strings.Split(frame, "\n")); got != rows {
					t.Errorf("at %dx%d the frame is %d lines, want %d", cols, rows, got, rows)
				}
			}
		}
	}
}

// A narrow terminal drops the address column rather than wrapping the table, and never
// produces a line wider than it has room for.
func TestNarrowTerminals(t *testing.T) {
	for _, cols := range []int{24, 32, 40, 50, 60, 70, 80} {
		frame := render(sample(), 24, cols)

		if frame == "terminal too small" {
			continue
		}

		for i, line := range strings.Split(frame, "\n") {
			if n := visibleLen(line); n > cols {
				t.Errorf("at %d columns line %d is %d wide: %q", cols, i, n, stripColour(line))
			}
		}
	}
}

// The selection stays on screen when the list is longer than the table.
func TestScrollingKeepsTheSelectionVisible(t *testing.T) {
	m := model{version: "v"}
	for i := range 40 {
		m.rows = append(m.rows, row{Sandbox: "s", Service: "svc" + string(rune('a'+i%26))})
	}

	m.selected = 39

	frame := stripColour(render(m, 20, 100))

	if !strings.Contains(frame, "›") {
		t.Error("the selected row scrolled off the screen, so the cursor is invisible")
	}
}
