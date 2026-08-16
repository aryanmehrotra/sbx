package ui

// How the screen is divided.
//
// One screen, always. There are no sub-screens to navigate into and back out of: everything
// worth seeing is visible at once, and what changes is the *content* of the bottom pane, not
// the shape of the page. A layout that rearranges itself as you move through it costs the
// reader their bearings every time, and the whole point of a dashboard is that you can glance
// at it.
//
// The table takes whatever is left. The first version pinned the table, the events and the
// footer at fixed heights and padded the middle with blank lines, which on a tall terminal
// produced a screen that was mostly nothing - it looked like the program had stopped.
//
// Small terminals lose panes from the bottom up, in the order they are worth least: the log
// pane first, then the detail line, then the column headers. The table and the key hints are
// the last things standing, because a dashboard you cannot read and cannot quit is worse than
// no dashboard.

// layout is the resolved geometry for one frame.
type layout struct {
	tableRows  int  // sandbox rows the table can show
	detailRows int  // lines for the selected sandbox; 0 hides it
	paneRows   int  // content lines for the bottom pane; 0 hides it
	header     bool // the column headings
	footer     bool // the key hints
}

// Fixed costs, in lines. Named rather than inlined because the arithmetic below only works if
// they are counted once and counted right.
const (
	lineTitle   = 1 // sbx, counts, update notice
	lineRule    = 1 // one horizontal rule
	lineHeader  = 1 // column headings
	linePaneTag = 1 // EVENTS / LOGS
	lineFooter  = 1 // key hints

	// detailFull is the expanded block: the name and ref, address and connect, then cpu and
	// memory with their recent history.
	//
	// Five, down from seven. `state` went because the table's STATE column already says it,
	// `ref` moved onto the title line, and address and connect share a line where there is
	// room - which bought the two graphs their space without the block growing.
	detailFull = 5
)

// wantDetail is the tallest the detail block could usefully be for this model.
//
// Every line is worth drawing now, awake or asleep: a sleeping service's usage line is the
// most interesting shape it has - it shows the drop to zero and when it happened - so there
// is no longer a case where the block would be padded with blanks.
func wantDetail(model) int { return detailFull }

// plan divides rows between the table and the panes below it.
//
// Spare space goes into detail rather than into padding. With four sandboxes on a forty-row
// terminal there is not forty rows of table to show, and the first version spent the
// difference on blank lines - which is how a working dashboard came to look like a stopped
// one. The same space spent on the selected sandbox's address, its connect command and its
// usage over time is space that answers the next question instead.
func plan(rows, items, want int) layout {
	l := layout{header: true, footer: true}

	fixed := lineTitle + lineRule

	switch {
	case rows < 6:
		// Barely a window. The table and nothing else.
		l.header, l.footer = false, false
		l.tableRows = max(1, rows-fixed)

		return l

	case rows < 9:
		l.tableRows = max(1, rows-fixed-lineHeader-lineFooter)

		return l

	case rows < 14:
		// One line of detail is worth its rule; a log pane is not worth four lines of chrome
		// to show two lines of log.
		l.detailRows = 1
		l.tableRows = max(1, rows-fixed-lineHeader-lineRule-l.detailRows-lineFooter)

		return l
	}

	fixed += lineHeader + lineRule + lineRule + linePaneTag + lineRule + lineFooter

	free := rows - fixed

	// The table asks for what it has, with a floor so that adding one sandbox does not shift
	// every pane below it, and a ceiling so a long list does not crowd everything else out.
	table := clamp(items, 5, max(5, free*2/3))

	rest := free - table

	// Detail grows to its full block before the pane takes anything above its own minimum.
	detail := clamp(rest-3, 1, want)

	l.detailRows = detail
	l.paneRows = max(3, rest-detail)

	// Whatever is still unclaimed goes to the table, so the screen ends up full.
	if used := table + detail + l.paneRows; used < free {
		table += free - used
	}

	l.tableRows = table

	return l
}

func clamp(v, lo, hi int) int {
	return max(lo, min(hi, v))
}
