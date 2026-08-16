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

	// detailFull is the expanded block: the name, then address, connect, state, cpu, memory
	// and ref.
	//
	// cpu and memory are the last two to be granted and the first two dropped, because they
	// are the only lines that are sometimes not worth drawing: a service with no ceilings set
	// has nothing to put in them that the table does not already show.
	detailFull = 7

	// detailWithMeters is the height at which the meters start to earn their space. Below it
	// the block keeps its original four fields and folds the memory figure back into the
	// state line, rather than dropping `ref` to make room for a bar.
	detailWithMeters = 6

	// detailNoMeters is the block as it was before the meters existed: name, address,
	// connect, state, ref. What a sleeping service is worth, since it has no usage to meter.
	detailNoMeters = 5
)

// plan divides rows between the table and the panes below it.
//
// Spare space goes into detail rather than into padding. With four sandboxes on a forty-row
// terminal there is not forty rows of table to show, and the first version spent the
// difference on blank lines - which is how a working dashboard came to look like a stopped
// one. The same space spent on the selected sandbox's address, its connect command and its
// state is space that answers the next question instead.
// wantDetail is the tallest the detail block could usefully be for this model.
//
// It exists because the block is not always worth its full height: the cpu and memory meters
// only have something to say about a service that is awake, and a layout that reserved their
// two lines regardless would leave two blank ones under a sleeping sandbox - which is the
// padding this file's own history says makes a working dashboard look stopped.
func wantDetail(m model) int {
	if r, ok := m.currentRow(); ok && r.Awake {
		return detailFull
	}

	return detailNoMeters
}

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
