package ui

// Turning the model into a frame.
//
// Pure: state in, string out, no terminal involved. Every hard case here - a name longer than
// its column, a terminal eighty columns wide, more sandboxes than rows on screen - is a test
// rather than something found by resizing a window and squinting at it.
//
// The frame is always exactly as tall as the terminal. Shorter leaves the tail of the previous
// frame on screen; taller scrolls the header off and never brings it back.

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// fieldIndent is what a detail line spends before its value: three spaces, a nine-column key
// and one more space. Named so the lines that have to fit inside what is left can say so.
const fieldIndent = 13

// What a trend line spends before the graph: the reading, then the percentage.
const (
	readingCols = 16
	pctCols     = 5
)

// messageLife is how long feedback from a keypress stays in the footer before the key hints
// come back. Long enough to read, short enough that the hints are never gone for good.
const messageLife = 4 * time.Second

// Colours, as escape sequences. Kept together so the palette is one thing rather than
// scattered through the layout, and so a future --no-colour has one place to switch off.
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[38;5;245m"
	green  = "\x1b[38;5;77m"
	cyan   = "\x1b[38;5;80m"
	yellow = "\x1b[38;5;179m"
	red    = "\x1b[38;5;167m"
	invert = "\x1b[7m"

	// The ground the whole dashboard is painted on.
	//
	// A terminal program that only sets foreground colours inherits whatever the reader's
	// theme is behind it, and on a light or a busy background the result is a table that does
	// not read as one thing. Painting every line to the full width makes the dashboard a panel
	// rather than text that happens to be on the screen.
	//
	// 234 is a near-black grey rather than true black: on an OLED-black terminal, true black
	// would make the panel invisible against the surround, and the point is that it is a
	// surface. SBX_UI_PLAIN=1 turns all of this off for anyone whose terminal or palette
	// disagrees.
	background = "\x1b[48;5;234m"

	// The ground the selected row stands on: the same surface, a few shades up. Light enough
	// to find at a glance, dark enough that the row's own colours still read against it.
	selection  = "\x1b[48;5;238m"
	clearRight = "\x1b[K"
)

// plainTheme reports whether to skip the painted background and use the terminal's own colours.
func plainTheme() bool { return os.Getenv("SBX_UI_PLAIN") != "" }

// paint puts a line on the dashboard's ground, padded so the surface reaches the right edge.
//
// Every inner reset has the background re-asserted after it. \x1b[0m resets *everything*,
// background included, so a line that sets a colour and clears it - which is every row here -
// lost the painted ground from that point on and finished on the terminal's own background.
// On screen that is a lighter band running from the state column to the right edge of the
// table, which looks like a rendering fault because it is one.
func paint(line string, cols int) string {
	if plainTheme() {
		return line
	}

	// A row that is already standing on the selection's ground is finished: painting it again
	// would swap that ground back to the panel's and lose the mark on the selected row.
	if strings.HasPrefix(line, selection) {
		return line
	}

	line = strings.ReplaceAll(line, reset, reset+background)

	if gap := cols - visibleLen(line); gap > 0 {
		line += strings.Repeat(" ", gap)
	}

	return background + line + reset
}

func render(m model, rows, cols int) string {
	if cols < 24 || rows < 4 {
		return "terminal too small"
	}

	shown := m.view()

	l := plan(rows, len(shown), wantDetail(m), wantPane(m))
	w := widths(shown, cols)

	var out []string

	add := func(s string) { out = append(out, paint(truncate(s, cols), cols)) }
	rule := func() { out = append(out, paint(dim+strings.Repeat("─", cols)+reset, cols)) }

	add(title(m, cols))
	rule()

	if l.header {
		add(tableHeader(m, w))
	}

	out = append(out, painted(tableRows(m, w, l.tableRows, cols), cols)...)

	if l.detailRows > 0 {
		rule()
		out = append(out, painted(detailBlock(m, w, l.detailRows, cols), cols)...)
	}

	if l.paneRows > 0 {
		rule()
		add(paneTitle(m, l.paneRows, cols))
		out = append(out, painted(paneBody(m, l.paneRows, cols), cols)...)
	}

	if l.footer {
		rule()
		add(footer(m, l.paneRows, cols))
	}

	for len(out) < rows {
		out = append(out, paint("", cols))
	}

	return strings.Join(out[:rows], "\n")
}

// title is the top line: what this is, how much of it there is, and anything urgent.
func title(m model, cols int) string {
	sandboxes, services, awake := m.counts()

	left := fmt.Sprintf(" %ssbx%s %s%s%s", bold, reset, dim, m.version, reset)

	// Both numbers count services, and the denominator says so. Without it the two figures
	// were a sandbox count and a service count sitting side by side, which is how "3
	// sandboxes · 4 awake" came to be printed on a screen listing seven services.
	right := fmt.Sprintf("%s · %d of %s awake",
		plural(sandboxes, "sandbox"), awake, plural(services, "service"))

	// Spelling it out costs about twenty columns, which a narrow terminal does not have. The
	// short form keeps the denominator and drops the words, because "4/7" still cannot be
	// misread as a count of sandboxes.
	if visibleLen(left)+len(right)+len(m.provider)+4 > cols {
		right = fmt.Sprintf("%d sbx · %d/%d awake", sandboxes, awake, services)
	}

	// What the fleet is costing, against what the machine has. A memory figure on its own is a
	// number; the same figure beside the ceiling it is heading for is a decision about whether
	// to sleep something. Dropped first when the line will not fit, because it is context and
	// the counts are the subject.
	// Two different machines, in the order somebody reads them: what the fleet is costing, then
	// what the computer it is on has left. Each is added only if it fits, and the laptop's goes
	// first when it does not - the fleet's share is the subject, the machine is the context.
	for _, seg := range []string{hostShare(m), machineFree(m)} {
		if seg == "" {
			continue
		}

		if visibleLen(left)+len(right)+len(seg)+len(m.provider)+7 > cols {
			continue
		}

		right += " · " + seg
	}

	if m.provider != "" {
		right += " · " + m.provider
	}

	if m.update != "" {
		right = fmt.Sprintf("%s%s available%s · %s", yellow, m.update, reset, right)
	}

	return pad(left, right+" ", cols)
}

// hostShare is the fleet's memory against the machine's, and the machine's cores.
//
// The machine as the containers see it: on macOS and Windows that is the VM, which is what they
// are actually sharing. A Mac with 32 GB whose colima was given 8 is a machine where 6 GB of
// sandboxes is nearly full, and measuring against 32 would be a comforting number about nothing.
func hostShare(m model) string {
	if m.host.MemBytes == 0 {
		return ""
	}

	var used uint64

	for _, r := range m.rows {
		if r.MemKnown {
			used += r.MemBytes
		}
	}

	// Named, because the next segment is a different machine's memory and an unlabelled pair of
	// figures beside each other reads as one machine described twice. The core counts stay in
	// the system pane, where each is on the line of the machine it belongs to.
	return fmt.Sprintf("sbx %s of %s", shortBytes(used), shortBytes(m.host.MemBytes))
}

// machineFree is what the computer the person is sitting at has left.
//
// Free rather than used, because the decision it informs is whether there is room for another
// sandbox - and on macOS the VM's ceiling and the laptop's are different numbers, so this one
// says which machine it is about.
func machineFree(m model) string {
	if m.machine.MemBytes == 0 || m.machine.FreeBytes == 0 {
		return ""
	}

	return fmt.Sprintf("host %s free of %s",
		shortBytes(m.machine.FreeBytes), shortBytes(m.machine.MemBytes))
}

func tableHeader(m model, w cols) string {
	// The second column is a service's name, or - grouped - what the sandbox holds. Leaving it
	// as SERVICE over a cell reading "2 services" is a header describing the wrong noun.
	what := "SERVICE"
	if m.grouped {
		what = "CONTAINS"
	}

	head := fmt.Sprintf("%s  %-*s  %-*s  %-6s  %*s  %*s",
		dim, w.sandbox, "SANDBOX", w.service, what, "STATE", w.cpu, "CPU", w.mem, "MEMORY")

	if w.address > 0 {
		head += "  " + fmt.Sprintf("%-*s", w.address, "ADDRESS")
	}

	if w.meters > 0 {
		head += "  " + fmt.Sprintf("%-*s  %-*s", meterCell, "CPU LIMIT", meterCell, "MEMORY LIMIT")
	}

	return strings.TrimRight(head, " ") + reset
}

// tableRows renders the visible slice of the table, scrolled to keep the selection in view.
func tableRows(m model, w cols, space, cols int) []string {
	var out []string

	shown := m.view()

	if len(shown) == 0 {
		// A failed listing is not evidence of an empty fleet: nothing was found because
		// nothing could be looked at, and saying both at once tells somebody whose docker is
		// down that they have no sandboxes.
		if m.err != nil {
			out = append(out, truncate(red+"  could not read the fleet - see below"+reset, cols))

			return padTo(out, space, cols)
		}

		// Every key along the bottom acts on a row, and there are none - so an empty fleet is
		// the one screen where the dashboard has nothing to offer and has to say what does.
		// One line saying "none" left a screen that answered a question nobody asked.
		for _, line := range emptyHelp(m, cols) {
			if len(out) >= space {
				break
			}

			out = append(out, line)
		}

		return padTo(out, space, cols)
	}

	start := 0
	if m.selected >= space {
		start = m.selected - space + 1
	}

	for i := start; i < len(shown) && len(out) < space; i++ {
		line := truncate(renderRow(shown[i], m.limitsFor(shown[i]), m.metered, i == m.selected, w), cols)

		if i == m.selected {
			line = highlight(line, cols)
		}

		out = append(out, line)
	}

	// Padding keeps the panes below from walking up and down the screen as sandboxes come and
	// go, which is what makes a table feel unsteady to read.
	for len(out) < space {
		out = append(out, "")
	}

	return out[:space]
}

// sandboxDetail is what a sandbox row shows below the table: the services it holds.
//
// The table row is a total - two services, 7% of a core, 365 MB - and a total is the one thing
// that cannot tell you which of them is the expensive one. So the block underneath is the list
// the grouped row folded up, with the addresses, because those are what somebody is looking for
// when they have picked a sandbox out of a list.
func sandboxDetail(m model, r row, w cols, space, cols int) []string {
	head := fmt.Sprintf(" %s%s%s  %s%s%s", cyan, r.Sandbox, reset, dim, r.Service, reset)
	connect := fmt.Sprintf("%seval \"$(sbx env %s)\"%s", dim, r.Sandbox, reset)

	if space == 1 {
		return []string{truncate(pad(head, connect+" ", cols), cols)}
	}

	out := []string{
		truncate(pad(head, connect+" ", cols), cols),
	}

	field := func(k, v string) {
		out = append(out, truncate(fmt.Sprintf("   %s%-9s%s %s", dim, k, reset, v), cols))
	}

	// The sandbox's own trace: its services added together at each moment, against the sum of
	// their ceilings. Sized exactly as the per-service one, so switching views with `v` moves
	// the numbers and not the columns they sit in.
	graph := cols - fieldIndent - readingCols - pctCols
	cpuLine, memLine := trendPair(m, r, graph)

	field("cpu", cpuLine)
	field("memory", memLine)

	// Then what it is made of. A total cannot say which service is the expensive one, and this
	// is the list that can.
	members := m.membersOf(r.Sandbox)

	// One grid for the whole block, every column as wide as the widest thing in it.
	//
	// Nothing is cut to make the sums work. A service publishing two ports carries both, and a
	// name nobody shortened stays whole - so the columns are measured from what is actually
	// there rather than from a constant somebody guessed, which is the difference between a
	// table and a list of lines that happen to have spaces in them.
	g := memberGrid(members, w)

	if len(members) > 0 && len(out) < space {
		out = append(out, memberHeader(g, cols))
	}

	for i, s := range members {
		room := space - len(out)
		if room <= 0 {
			break
		}

		// On the last line it can print, say what it could not. Stopping mid-list leaves a
		// sandbox that says "5 services" above a list of four, which reads as a service that
		// has gone missing rather than a screen that ran out of room.
		if room == 1 && len(members)-i > 1 {
			out = append(out, truncate(fmt.Sprintf("   %s… and %d more - press v for the "+
				"service view%s", dim, len(members)-i, reset), cols))

			break
		}

		out = append(out, truncate(memberLine(m, s, r, g, cols), cols))
	}

	return padTo(out, space, cols)
}

// memberLine is one service inside its sandbox, with its numbers under the table's own.
//
// Under them literally. The table above says CPU and MEMORY over two columns, and the same two
// figures for the services inside a sandbox were printed a few characters to the right of them -
// close enough to look like a mistake and far enough that the eye cannot read down the column.
// So the line is padded to end its readings exactly where the row above ends its own, which is
// arithmetic the table already did in widths().
//
// Where the name, state and address have already run past that point - a long service name on a
// narrow terminal - the readings follow on after a space instead. Alignment is worth having and
// not worth overlapping the address to get.
func memberLine(m model, s row, sandbox row, g grid, width int) string {
	state, colour := "asleep", dim
	if s.Awake {
		state, colour = "AWAKE", green
	}

	cpu, mem, share := "", "", ""

	if s.Awake {
		// A sample that has not arrived is not zero, and says so differently.
		cpu, mem = "…", "…"

		if s.CPUKnown {
			cpu = millicores(s.CPU)
		}

		if s.MemKnown {
			mem = shortBytes(s.MemBytes)

			// Of the sandbox's own total rather than of a ceiling: the question is which service
			// the memory went to, and most services have no ceiling to be a percentage of.
			if sandbox.MemKnown && sandbox.MemBytes > 0 {
				share = fmt.Sprintf("%.0f%%", 100*float64(s.MemBytes)/float64(sandbox.MemBytes))
			}
		}
	}

	// Every cell padded to the block's own width, so each column is where the heading says it is
	// on every row - including the row whose address is twice as wide as the rest, which keeps
	// its address in full rather than being cut to make the arithmetic easier.
	line := fmt.Sprintf("   %-*s  %s%-*s%s  %*s  %*s  %s%*s%s  %s%-*s%s",
		g.service, s.Service, colour, g.state, state, reset,
		g.cpu, cpu, g.mem, mem, dim, g.share, share, reset, dim, g.addr, s.Address, reset)

	// Whatever is left, and only where it is enough to draw a shape rather than a smudge.
	if room := width - visibleLen(line) - 2; room >= 12 && s.Awake {
		var values []float64
		for _, sample := range m.series[s.Ref] {
			values = append(values, float64(sample.mem))
		}

		if trace := spark(values, 0, min(room, 44)); trace != "" {
			line += "  " + dim + trace + reset
		}
	}

	return line
}

// systemBody is the machine: what is on it, what each is holding, and what is left.
//
// Everything, not only ours. "What is using the memory" is rarely answered by the sandboxes
// alone - a laptop's runtime holds whatever else the day has left there - and a pane that
// listed only sbx's services would report a nearly empty machine while the VM was full.
func systemBody(m model, space, cols int) []string {
	if len(m.neighbours) == 0 {
		return []string{truncate(dim+"  reading the machine…"+reset, cols)}
	}

	sorted := append([]provider.Neighbour(nil), m.neighbours...)

	// By what they are holding, because the question is which of them to stop.
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].MemBytes != sorted[j].MemBytes {
			return sorted[i].MemBytes > sorted[j].MemBytes
		}

		return sorted[i].Name < sorted[j].Name
	})

	var (
		out          []string
		ours, others uint64
		asleep       int
	)

	for _, n := range sorted {
		if !n.Running {
			asleep++
		}

		if n.Ours {
			ours += n.MemBytes
		} else {
			others += n.MemBytes
		}
	}

	// Both machines first, in the order somebody reads them. On macOS and Windows these are
	// genuinely two computers and the numbers are not comparable: the laptop reports what is
	// free because its kernel knows, and the runtime reports only what the containers hold,
	// because docker cannot say what else is inside the VM.
	if m.machine.MemBytes > 0 {
		out = append(out, truncate(fmt.Sprintf("  %s%-16s%s %s%s · %s free of %s%s",
			reset, "this machine", reset, dim,
			plural(m.machine.Cores, "core"),
			shortBytes(m.machine.FreeBytes), shortBytes(m.machine.MemBytes), reset), cols))
	}

	if m.host.MemBytes > 0 {
		name := m.host.Name
		if name == "" {
			name = "the runtime"
		}

		out = append(out, truncate(fmt.Sprintf("  %s%-16s%s %s%s · containers hold %s of %s   "+
			"sbx %s · other %s%s",
			reset, name, reset, dim,
			plural(m.host.Cores, "core"),
			shortBytes(ours+others), shortBytes(m.host.MemBytes),
			shortBytes(ours), shortBytes(others), reset), cols))
	}

	// One width for the names, taken from the names that are actually there. A fixed column is
	// exact right up until something overflows it, and then that row and every wider one after
	// it carry their numbers a few characters further right - which is precisely what a list of
	// containers named sbx-<sandbox>-<service> does, and what makes this read as a ragged list
	// rather than a table.
	nameCol := 0

	for _, n := range sorted {
		if n.Running {
			nameCol = max(nameCol, visibleLen(n.Name))
		}
	}

	nameCol = clamp(nameCol, len("CONTAINER"), max(len("CONTAINER"), cols-30))

	memCol := len("MEMORY")
	for _, n := range sorted {
		if n.Running {
			memCol = max(memCol, len(shortBytes(n.MemBytes)))
		}
	}

	if len(out) < space {
		out = append(out, truncate(fmt.Sprintf("  %s%-*s  %*s  %-*s  %s%s",
			dim, nameCol, "CONTAINER", memCol, "MEMORY", tableBarCells+2, "SHARE", "WHOSE",
			reset), cols))
	}

	for _, n := range sorted {
		if len(out) >= space {
			break
		}

		// A stopped container holds nothing, which is the point of the project - so they are
		// counted in a line at the end rather than given one each.
		if !n.Running {
			continue
		}

		who := ""
		if n.Ours {
			who = "  " + green + "sbx" + reset
		}

		bar := "  " + strings.Repeat(" ", tableBarCells+2)
		if m.host.MemBytes > 0 {
			bar = "  " + dim + smallBar(float64(n.MemBytes)/float64(m.host.MemBytes)) + reset
		}

		out = append(out, truncate(fmt.Sprintf("  %-*s  %s%*s%s%s%s",
			nameCol, n.Name, reset, memCol, shortBytes(n.MemBytes), reset, bar, who), cols))
	}

	if asleep > 0 && len(out) < space {
		out = append(out, truncate(fmt.Sprintf("  %s%d asleep, holding nothing%s", dim, asleep, reset), cols))
	}

	return out
}

// grid is a block's column widths, measured from what is in it.
//
// A dashboard reads as a table only when every row agrees where its columns are, and the way to
// guarantee that is to take the widths from the data once and hand the same numbers to every
// line. Constants cannot do it: they are exact until the first thing that does not fit, and
// then every wider row after it is off by however much it overflowed.
type grid struct {
	service, state, addr int
	cpu, mem, share      int
	readingsAt           int // the column the numbers begin at
}

// memberGrid measures the block's columns, and puts the three it shares with the table above
// exactly where the table puts them.
//
// STATE, CPU and MEMORY mean the same thing in both, so a reader should be able to run their eye
// down one column rather than find it again in each block. It costs nothing: the service name
// takes the width the table spends on a sandbox name and what it contains, which is more than a
// service name needs anyway.
//
// The two columns the table has no equivalent for - the address, and the service's share of its
// sandbox - go after them, in the room the table gives to its limit columns.
func memberGrid(members []row, w cols) grid {
	g := grid{service: len("SERVICE"), state: max(len("asleep"), 6), addr: len("ADDRESS"),
		cpu: max(len("CPU"), w.cpu), mem: max(len("MEMORY"), w.mem), share: len("SHARE")}

	for _, s := range members {
		g.service = max(g.service, visibleLen(s.Service))
		g.addr = max(g.addr, visibleLen(s.Address))

		if s.CPUKnown {
			g.cpu = max(g.cpu, len(millicores(s.CPU)))
		}

		if s.MemKnown {
			g.mem = max(g.mem, len(shortBytes(s.MemBytes)))
		}
	}

	// The table spends "marker, space, sandbox, gap, contains" before its STATE column and the
	// block spends "three spaces, service"; widening the service column by the difference is what
	// lines the two up. Where a service name is longer than that the name wins and this sandbox
	// gives up the alignment, because a name cut to make two columns agree is a worse trade.
	g.service = max(g.service, 2+w.sandbox+2+w.service-3)

	g.readingsAt = 3 + g.service + 2 + g.state + 2

	return g
}

// millicores is cpu the way a cluster states it: thousandths of one core.
//
// A percentage answers "is it busy" and cannot be added up or held against a limit somebody
// wrote as "500m" in a spec. The suffix is "mc" rather than the bare "m" kubernetes uses because
// the column beside this one prints "563m" for megabytes, and two columns of numbers ending in
// the same letter meaning different things is worse than two extra characters.
func millicores(percentOfOneCore float64) string {
	return fmt.Sprintf("%.0fmc", percentOfOneCore*10)
}

// memberHeader names the columns under a sandbox, because a column of figures with nothing over
// it is a number the reader has to identify from its shape.
func memberHeader(g grid, cols int) string {
	return truncate(fmt.Sprintf("   %s%-*s  %-*s  %*s  %*s  %*s  %-*s%s",
		dim, g.service, "SERVICE", g.state, "STATE",
		g.cpu, "CPU", g.mem, "MEMORY", g.share, "SHARE", g.addr, "ADDRESS", reset), cols)
}

// maxSystemName caps the name column, so one container with a very long name cannot spend the
// whole width on saying so and leave no room for the figure beside it.
const maxSystemName = 44

// maxMemberAddr caps what one address may take from the line. Two loopback addresses fit; a
// service publishing five does not get to spend the whole width on saying so.
const maxMemberAddr = 32

// memberPrefix is what a member line spends before its address: three spaces, the service name,
// a space, the state and the gap after it.
const memberPrefix = 3 + 14 + 1 + 6 + 2

// endAt pads a line so the token ends exactly at col, or - where the line has already run past
// that point - puts it after a gap instead.
//
// The gap is the part that matters. Padding to a column already behind you pads by nothing, and
// the token then lands hard against whatever was there: with a short sandbox name the cpu column
// sits left of where the address ends, and "127.0.0.1:1" and "5.0%" were printed as
// "127.0.0.1:15.0%". Alignment is worth having and never worth running two fields together.
func endAt(line, token string, col int) string {
	if want := col - visibleLen(token); visibleLen(line) < want {
		return padRight(line, want) + token
	}

	return line + "  " + token
}

// padTo keeps the panes below from walking up and down the screen as sandboxes come and go,
// which is what makes a table feel unsteady to read.
func padTo(out []string, space, _ int) []string {
	for len(out) < space {
		out = append(out, "")
	}

	return out[:space]
}

// emptyHelp is what the table says when there is nothing in it.
//
// An empty fleet is the one screen where every key along the bottom - wake, sleep, logs, limit,
// remove - acts on a row that does not exist. Answering "no sandboxes yet" and stopping leaves
// somebody looking at a full screen of nothing with no idea what to do next, which is the
// moment a dashboard is least useful and most easily made useful.
//
// The history line is there because sandboxes outlive nothing and their history outlives them:
// a machine whose containers were cleared still knows what ran, and "there are none" reads like
// amnesia when the record is right there.
func emptyHelp(m model, cols int) []string {
	line := func(colour, s string) string { return truncate(colour+s+reset, cols) }

	// The table pane is a handful of rows on an ordinary terminal, so this has to say the most
	// useful thing first: anything past the fifth line is cut off and never seen.
	head := "  nothing here yet - no sandboxes on this machine."
	if n := len(m.events); n > 0 {
		head = fmt.Sprintf("  no sandboxes on this machine - %s below outlived them.", plural(n, "event"))
	}

	// Assembled first and truncated once. Truncating the command and its description
	// separately lets each one have the full width, so on a narrow terminal the pair came to
	// twice it - which the every-key-in-every-state test caught at 30 columns.
	//
	// The gap is spelled out rather than left to the padding: the longest command here is
	// exactly the column width, so %-34s alone runs its description straight into it.
	do := func(cmd, why string) string {
		return truncate(reset+fmt.Sprintf("    %-34s", cmd)+dim+"  "+why+reset, cols)
	}

	out := []string{
		line(dim, head),
		"",
		do("sbx init", "a template, a sandbox.json, and go"),
		do("sbx create dev --template postgres", "one right now, nothing needed on disk"),
	}

	// Only where there is a record to point at. Offering it on a machine that has never run
	// anything would be sending somebody to an empty file.
	if len(m.events) > 0 {
		out = append(out, do("sbx history", "everything that has run here, still"))
	}

	return out
}

func renderRow(r row, l provider.Limits, metered, selected bool, w cols) string {
	marker := " "
	if selected {
		marker = "›"
	}

	state, colour := "asleep", dim
	if r.Awake {
		state, colour = "AWAKE", green
	}

	// A sandbox is not simply awake or asleep - two of its four services can be. "AWAKE" on a
	// row standing for four things, one of which is running, is a fair summary of the cost and
	// a bad summary of the state, so the row says which it means.
	if r.Members > 0 {
		state = fmt.Sprintf("%d/%d up", r.Woken, r.Members)

		switch {
		case r.Woken == 0:
			colour = dim
		case r.Woken < r.Members:
			colour = yellow
		default:
			colour = green
		}
	}

	// An asleep service shows a dash rather than 0. Zero is a measurement and this is not one:
	// there is nothing running to measure, which is the point of the project. A sample that
	// has not arrived yet is not zero either, and says so differently.
	cpu, mem := "-", "-"

	if r.Awake {
		// "…" promises a sample is coming. On a backend with no metrics none ever is, and a
		// row that says "wait" forever is worse than one that says it cannot know.
		cpu, mem = "…", "…"
		if !metered {
			cpu, mem = "n/a", "n/a"
		}

		if r.CPUKnown {
			cpu = fmt.Sprintf("%.1f%%", r.CPU)
		}

		if r.MemKnown {
			mem = humanBytes(r.MemBytes)
		}
	}

	line := fmt.Sprintf("%s %-*s  %-*s  %s%-6s%s  %*s  %*s",
		marker,
		w.sandbox, truncateName(r.Sandbox, w.sandbox),
		w.service, truncateName(r.Service, w.service),
		colour, state, reset,
		w.cpu, cpu,
		w.mem, mem)

	if w.address > 0 {
		addr := truncate(shortAddress(r.Address, w.address), w.address)
		line += "  " + dim + addr + reset

		if w.meters > 0 {
			line += strings.Repeat(" ", max(0, w.address-visibleLen(addr)))
		}
	}

	if w.meters > 0 {
		line += "  " + cell(r.CPUKnown && r.Awake, r.CPU/100, float64(l.NanoCPUs)/1e9,
			trimZeros(float64(l.NanoCPUs)/1e9)+"c") +
			"  " + cell(r.MemKnown && r.Awake, float64(r.MemBytes), float64(l.MemBytes),
			shortBytes(l.MemBytes))
	}

	return line
}

// cell draws one row's meter: a bar of how full, and the ceiling it is full of.
//
// An uncapped service gets a dash rather than an empty bar. An empty bar against no limit
// would be a proportion of nothing, drawn as though it meant something.
func cell(sampled bool, used, allowed float64, label string) string {
	if allowed <= 0 {
		// An ASCII dash, not an em dash. Every column here is placed by counting characters,
		// and that arithmetic is only true if the terminal draws each one in a single cell.
		// U+2014 is East Asian Ambiguous, which iTerm2 and others render double-width by
		// default - so a table that measured perfectly still looked a character out from the
		// dash onward, and the fault was in the glyph rather than in the sums.
		return dim + fmt.Sprintf("%-*s", meterCell, "-") + reset
	}

	// Not sampled yet is not the same as not capped: the ceiling is known and worth showing,
	// it is only the fill that is missing.
	if !sampled {
		return dim + "[" + strings.Repeat(".", tableBarCells) + "]" + reset +
			" " + dim + fmt.Sprintf("%*s", limitTextCols, label) + reset
	}

	return smallBar(used/allowed) + " " + dim + fmt.Sprintf("%*s", limitTextCols, label) + reset
}

// smallBar is the table's bar: the detail block's, at the width a column can afford.
func smallBar(frac float64) string {
	filled := int(frac * tableBarCells)

	switch {
	case filled < 1 && frac > 0:
		filled = 1
	case filled > tableBarCells:
		filled = tableBarCells
	case frac <= 0:
		filled = 0
	}

	colour := green

	switch {
	case frac >= 0.9:
		colour = red
	case frac >= 0.75:
		colour = yellow
	}

	return dim + "[" + reset + colour + strings.Repeat("█", filled) + reset +
		dim + strings.Repeat(".", tableBarCells-filled) + "]" + reset
}

// shortBytes writes a ceiling the compact way a column has room for: "512m", "4g".
func shortBytes(b uint64) string {
	switch {
	case b == 0:
		return "-"
	case b >= 1<<30:
		return trimZeros(float64(b)/(1<<30)) + "g"
	case b >= 1<<20:
		// Whole megabytes. "17.47m" is four characters of noise about a number that moves
		// every second anyway.
		return strconv.FormatUint(b/(1<<20), 10) + "m"
	default:
		return strconv.FormatUint(b/(1<<10), 10) + "k"
	}
}

// highlight marks the selected row, across the full width.
//
// Padded before it is inverted, not after: a highlight that stops where the text does looks
// like a rendering fault rather than a selection, and the painted background would otherwise
// fill the rest of the line and cut it short.
//
// The colours are stripped first because an inverted line that still carries its own
// foreground colours is unreadable on a good half of terminals.
// highlight marks the selected row by standing it on a lighter ground.
//
// It used to strip every colour off the line and invert it, which was fine when a row was
// text and became a bug the moment a row contained a bar: inverted, a solid block takes the
// background's colour, so a full meter on the selected row rendered as an empty one. The row
// somebody is looking at was the one row that lied about how full a service was.
//
// So the colours stay and the ground changes instead. Like paint, the selection has to be
// re-asserted after every inner reset, because a reset clears the background too.
func highlight(line string, cols int) string {
	if plainTheme() {
		// No palette to work with, so inversion is the only marker available - and with no
		// colour there are no bars to ruin.
		flat := stripColour(line)

		if gap := cols - visibleLen(flat); gap > 0 {
			flat += strings.Repeat(" ", gap)
		}

		return invert + flat + reset
	}

	line = strings.ReplaceAll(line, reset, reset+selection)

	if gap := cols - visibleLen(line); gap > 0 {
		line += strings.Repeat(" ", gap)
	}

	return selection + line + reset
}

// shortAddress drops the loopback prefix when the column is tight. Every docker address is
// 127.0.0.1, so the host repeated down the column is ten characters saying nothing.
func shortAddress(addr string, width int) string {
	if addr == "" || len(addr) <= width {
		return addr
	}

	return strings.ReplaceAll(addr, "127.0.0.1:", ":")
}

// detailBlock describes the selected sandbox.
//
// It is the answer to "and now what": the address to connect to, the command that exports it,
// and what the thing is actually doing. On a short terminal it collapses to the one line that
// matters; given room it expands, which is a better use of a tall screen than blank lines.
func detailBlock(m model, w cols, space, cols int) []string {
	r, ok := m.currentRow()
	if !ok {
		return blanks(space)
	}

	if r.Members > 0 {
		return sandboxDetail(m, r, w, space, cols)
	}

	head := fmt.Sprintf(" %s%s/%s%s  %s%s%s", cyan, r.Sandbox, r.Service, reset, dim, r.Ref, reset)

	right := ""
	if s, ok := m.stats[r.Sandbox+"/"+r.Service]; ok && s.wakes > 0 {
		right = fmt.Sprintf("woke %d× · last %dms ", s.wakes, s.lastWakeMs)
	}

	// One line: the name and the command, because that is what gets typed next.
	if space == 1 {
		line := fmt.Sprintf(" %s%s/%s%s   %seval \"$(sbx env %s)\"%s",
			cyan, r.Sandbox, r.Service, reset, dim, r.Sandbox, reset)

		return []string{truncate(pad(line, dim+right+reset, cols), cols)}
	}

	out := []string{truncate(pad(head, dim+right+reset, cols), cols)}

	field := func(k, v string) {
		out = append(out, truncate(fmt.Sprintf("   %s%-9s%s %s", dim, k, reset, v), cols))
	}

	connect := fmt.Sprintf("%seval \"$(sbx env %s)\"%s", dim, r.Sandbox, reset)

	// Two lines, always. Merging them where the terminal is wide enough was tempting and made
	// the block's height depend on its width, which is how a layout ends up padding itself
	// with a blank line on a wide screen and nowhere else.
	field("address", r.Address)
	field("connect", connect)

	// What is left for the trace and its legend, after the field label, the reading and the
	// percentage. Counted rather than estimated: the first version forgot the percentage
	// column, so the line overran the terminal by five and the truncate ate the span off the
	// end of the legend - which is the one number that says how far back the graph reaches.
	graph := cols - fieldIndent - readingCols - pctCols

	// Both rows are sized together. Sized apart, each subtracted its own legend from the
	// graph's width - and "peak 0.49c of 0.5c" is a column wider than "peak 475m of 512m", so
	// the two traces began one apart and the eye reads that as a rendering fault.
	cpuLine, memLine := trendPair(m, r, graph)

	field("cpu", cpuLine)
	field("memory", memLine)

	for len(out) < space {
		out = append(out, "")
	}

	return out[:space]
}

// trend is one metric: what it is now, against what it is allowed, and where it has been.
//
// The reading and the line answer different questions and the dashboard was only answering the
// first. "477 MB of 512 MB" is alarming or fine depending entirely on whether it has been
// there all afternoon or arrived in the last thirty seconds, and that is the half a table
// cannot show.
// trendPair renders the cpu and memory lines to one shared geometry, so their traces start and
// end in the same columns and their legends line up under each other.
func trendPair(m model, r row, width int) (string, string) {
	cpu := trendParts(m, r, true)
	mem := trendParts(m, r, false)

	// Sized against the whole series first, which gives the longest span the legend could
	// ever print, so deciding the width cannot depend on the width it decides.
	legendCols := max(
		visibleLen(legend(cpu.values, cpu.ceiling, true, len(cpu.values))),
		visibleLen(legend(mem.values, mem.ceiling, false, len(mem.values))),
	)

	cells := max(0, width-legendCols)

	// Then written for what is actually drawn: two samples to a cell.
	cpuLegend := legend(cpu.values, cpu.ceiling, true, min(len(cpu.values), cells*2))
	memLegend := legend(mem.values, mem.ceiling, false, min(len(mem.values), cells*2))

	line := func(t trendLine, leg string) string {
		if t.message != "" {
			return t.message
		}

		return padVisible(t.now, readingCols) + padVisible(t.pct, pctCols) +
			padLeft(spark(t.values, t.ceiling, cells), cells) + padRight(leg, legendCols)
	}

	return line(cpu, cpuLegend), line(mem, memLegend)
}

// trendLine is one metric's parts, before they are laid out.
type trendLine struct {
	message string // set when there is nothing to draw, and then it is the whole line
	now     string
	pct     string
	values  []float64
	ceiling float64
}

// trendParts works out one metric's reading, percentage, series and legend. Laying them out
// is trendPair's job, because the two rows have to agree on a geometry neither can see alone.
func trendParts(m model, r row, isCPU bool) trendLine {
	if !m.metered {
		return trendLine{message: dim + "this backend does not report usage" + reset}
	}

	l := m.limitsFor(r)

	var t trendLine

	for _, s := range m.seriesFor(r) {
		if isCPU {
			t.values = append(t.values, s.cores)
		} else {
			t.values = append(t.values, float64(s.mem))
		}
	}

	switch {
	case isCPU && l.NanoCPUs > 0:
		t.ceiling = float64(l.NanoCPUs) / 1e9
	case !isCPU && l.MemBytes > 0:
		t.ceiling = float64(l.MemBytes)
	}

	switch {
	case !r.Awake && !isCPU:
		// The one place the old block's reassurance survives. "is my data still there" is the
		// question a sleeping database raises, and the table's STATE column does not answer it.
		t.now = dim + "asleep · volume intact" + reset
	case !r.Awake:
		t.now = dim + "asleep" + reset
	case isCPU && r.CPUKnown && t.ceiling > 0:
		t.now = fmt.Sprintf("%.2f/%s", r.CPU/100, trimZeros(t.ceiling)+"c")
	case isCPU && r.CPUKnown:
		t.now = fmt.Sprintf("%.2fc", r.CPU/100)
	case !isCPU && r.MemKnown && t.ceiling > 0:
		t.now = shortBytes(r.MemBytes) + "/" + shortBytes(l.MemBytes)
	case !isCPU && r.MemKnown:
		t.now = shortBytes(r.MemBytes)
	default:
		t.now = dim + "…" + reset
	}

	// The percentage is the one figure a reader can compare across services without doing
	// arithmetic, and it only exists where there is a ceiling to be a percentage of.
	if t.ceiling > 0 && r.Awake {
		var used float64

		switch {
		case isCPU && r.CPUKnown:
			used = r.CPU / 100
		case !isCPU && r.MemKnown:
			used = float64(r.MemBytes)
		}

		if used > 0 {
			t.pct = fmt.Sprintf("%.0f%%", used/t.ceiling*100)

			switch frac := used / t.ceiling; {
			case frac >= 0.9:
				t.pct = red + t.pct + reset
			case frac >= 0.75:
				t.pct = yellow + t.pct + reset
			}
		}
	}

	return t
}

// legend says what the graph's height and length mean.
//
// "peak" rather than "max" because it is the highest reading in the window rather than a
// limit, and "of 0.5c" when there is a ceiling so full height is known to mean full. The span
// answers the other question a growing line raises, which is how far back it goes - it grows
// until it fills the width and then slides, and without a number nobody can tell which.
func legend(values []float64, ceiling float64, isCPU bool, shown int) string {
	if len(values) == 0 {
		return ""
	}

	var peak float64
	for _, v := range values {
		peak = max(peak, v)
	}

	span := shortDuration(time.Duration(shown) * Refresh)

	unit := func(v float64) string {
		if isCPU {
			return trimZeros(v) + "c"
		}

		return shortBytes(uint64(v))
	}

	scale := "peak " + unit(peak)
	if ceiling > 0 {
		scale = "peak " + unit(peak) + " of " + unit(ceiling)
	}

	return dim + "  " + scale + " · " + span + reset
}

// shortDuration writes a window the way somebody would say it: "45s", "3m", "1h".
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	default:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
}

// padLeft pads on the left, which is what puts the newest end of a trace against a fixed
// right edge.
func padLeft(s string, n int) string {
	if gap := n - visibleLen(s); gap > 0 {
		return strings.Repeat(" ", gap) + s
	}

	return s
}

// padRight pads on the right to an exact width, which is what keeps two legends starting in
// the same column when one of them is shorter than the other.
func padRight(s string, n int) string {
	if gap := n - visibleLen(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}

	return s
}

// padVisible pads to a width counted in what the reader sees, not in bytes. The readings
// carry colour, and %-*s counts escape sequences as though they were columns.
func padVisible(s string, n int) string {
	if gap := n - visibleLen(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}

	return s + " "
}

// brailleDots maps a position in a braille cell to the bit that lights it.
//
// A cell is two columns of four dots, which is what makes it the right glyph for a line: two
// samples fit side by side in the space one block character would take, and the dot can sit at
// a height rather than filling everything below it. The numbering is not sequential - dots 7
// and 8 were added to the standard later and kept the high bits - so it is written out.
var brailleDots = [4][2]rune{
	{0x01, 0x08}, // top
	{0x02, 0x10},
	{0x04, 0x20},
	{0x40, 0x80}, // bottom
}

const brailleBlank = 0x2800

// spark draws a series as one line of text.
//
// A trace rather than a bar chart. The block glyphs it used to draw filled everything below
// the reading, which turns a line that is merely high into a solid wall and hides the shape
// inside it - and shape is the entire reason the graph is there. Braille plots the reading
// where it happened and joins it to the one before, so a service holding steady near its
// ceiling looks different from one climbing towards it.
//
// Scaled to the ceiling when there is one, so height means fullness and two services with the
// same limit are directly comparable. Without a ceiling it scales to the window's own peak,
// which says nothing about how full anything is and everything about shape.
func spark(values []float64, ceiling float64, cells int) string {
	if cells < 4 {
		return ""
	}

	if len(values) == 0 {
		return dim + "no readings yet" + reset
	}

	// Two samples to a cell.
	if n := cells * 2; len(values) > n {
		values = values[len(values)-n:]
	}

	top := ceiling

	if top <= 0 {
		for _, v := range values {
			top = max(top, v)
		}
	}

	if top <= 0 {
		return dim + strings.Repeat("⠄", (len(values)+1)/2) + reset
	}

	// row 0 is the top of the cell, so a full reading is row 0 and an empty one row 3.
	row := func(v float64) int {
		return 3 - clamp(int(v/top*3+0.5), 0, 3)
	}

	// Every cell takes the colour of what was happening when it was drawn, not of what is
	// happening now. A single colour for the whole line says "this service is fine" over a
	// trace that spent two of its four minutes against the ceiling, and the moment it went bad
	// is the one thing a history is for.
	colourAt := func(v float64) string {
		if ceiling <= 0 {
			return green
		}

		switch frac := v / ceiling; {
		case frac >= 0.9:
			return red
		case frac >= 0.75:
			return yellow
		}

		return green
	}

	var (
		b    strings.Builder
		prev = row(values[0])
		cur  string
	)

	for i := 0; i < len(values); i += 2 {
		cell := rune(brailleBlank)
		worst := values[i]

		for half := range 2 {
			if i+half >= len(values) {
				break
			}

			at := row(values[i+half])
			worst = max(worst, values[i+half])

			// Joined to the sample before it. Without this a series that jumps leaves two
			// unrelated dots and reads as noise rather than as a line that moved.
			for r := min(prev, at); r <= max(prev, at); r++ {
				cell |= brailleDots[r][half]
			}

			prev = at
		}

		// The louder of the two readings in the cell, because a cell that touched the ceiling
		// should not be painted calm by the sample beside it.
		if c := colourAt(worst); c != cur {
			b.WriteString(c)

			cur = c
		}

		b.WriteRune(cell)
	}

	return b.String() + reset
}

// limitChoices offers the sizes, and gives way to the short form when the terminal is narrow.
//
// The numbers are the keys. Naming them in the footer rather than in a box that opens over the
// table keeps the fleet visible while somebody decides - the whole point of a dashboard is
// that a choice is made in front of the thing it is about.
func limitChoices(label string, cols int) string {
	named := cyan + "  " + label + reset + "  "
	for _, p := range limitPresets {
		named += dim + " " + string(p.key) + " " + reset + p.name + "   "
	}

	named += dim + " c " + reset + "custom" + dim + "   esc" + reset

	short := cyan + "  limit" + reset + "  "
	for _, p := range limitPresets {
		short += p.short + "  "
	}

	short += dim + "c custom  esc" + reset

	tight := "  "
	for _, p := range limitPresets {
		tight += p.short + " "
	}

	tight += dim + "c ✎ esc" + reset

	// Longest first. A footer that overflows is one that pushes the key hints off the right
	// edge of a terminal somebody chose to keep narrow.
	for _, s := range []string{named, short, tight} {
		if visibleLen(s) <= cols {
			return s
		}
	}

	return truncate(tight, cols)
}

// trimZeros writes a core count the way somebody would say it: "0.5", "2", not "2.00".
func trimZeros(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")

	return strings.TrimSuffix(s, ".")
}

// painted puts a block of already-built lines on the ground.
func painted(lines []string, cols int) []string {
	out := make([]string, 0, len(lines))

	for _, l := range lines {
		out = append(out, paint(l, cols))
	}

	return out
}

func blanks(n int) []string {
	out := make([]string, n)

	return out
}

func paneTitle(m model, space, cols int) string {
	name := "EVENTS"
	total := len(m.events)

	switch {
	case m.pane == paneLogs:
		name, total = "LOGS", len(m.logs)

		if r, ok := m.currentRow(); ok {
			name = "LOGS " + r.Sandbox + "/" + r.Service
		}

	case m.pane == paneSystem:
		name, total = "SYSTEM", len(m.neighbours)

		// What machine, named. On a laptop this is the VM rather than the laptop, and saying
		// which one keeps "7.7g" from reading as a claim about the Mac it is running on.
		if m.host.Name != "" {
			name = "SYSTEM · " + m.host.Name
		}
	}

	if m.focus == focusPane {
		name = "▸ " + name
	}

	right := "l logs · a system · tab focuses"

	// Only worth saying when there is more than fits, and only then is "following" meaningful.
	if total > space {
		shown := total - m.offset

		if m.offset == 0 {
			right = fmt.Sprintf("%d lines · following", total)
		} else {
			right = fmt.Sprintf("%d/%d · ↑↓ scroll · G follows", shown, total)
		}
	}

	return pad(" "+dim+name+reset, dim+right+reset+" ", cols)
}

// paneBody is the bottom pane: recent events, or the selected service's logs.
//
// A window into the content, positioned from the end. Offset 0 is the tail, which is where
// both a log and a history are interesting, and scrolling back moves the window without ever
// changing its size - so the screen does not reflow while somebody is reading it.
func paneBody(m model, space, cols int) []string {
	var raw []string

	if m.pane == paneSystem {
		return pad_lines(systemBody(m, space, cols), space)
	}

	if m.pane == paneLogs {
		if len(m.logs) == 0 {
			return pad_lines([]string{truncate(dim+"  no output yet"+reset, cols)}, space)
		}

		for _, l := range window(m.logs, space, m.offset) {
			raw = append(raw, truncate("  "+formatLog(l), cols))
		}
	} else {
		if len(m.events) == 0 {
			return pad_lines([]string{truncate(dim+"  nothing has happened yet"+reset, cols)}, space)
		}

		for _, e := range window(m.events, space, m.offset) {
			raw = append(raw, truncate(eventLine(e), cols))
		}
	}

	return pad_lines(raw, space)
}

// window returns the n items ending offset from the end.
func window[T any](s []T, n, offset int) []T {
	if n <= 0 || len(s) == 0 {
		return nil
	}

	end := len(s) - offset
	end = clamp(end, min(n, len(s)), len(s))

	start := max(0, end-n)

	return s[start:end]
}

// maxOffset is how far back the pane can be scrolled: far enough to reach the first line, and
// no further, so holding the key does not walk off into empty space.
func maxOffset(m model, space int) int {
	n := len(m.events)
	if m.pane == paneLogs {
		n = len(m.logs)
	}

	return max(0, n-space)
}

func pad_lines(lines []string, space int) []string {
	for len(lines) < space {
		lines = append(lines, "")
	}

	return lines[:space]
}

func eventLine(e history.Record) string {
	return fmt.Sprintf("%s  %-4s%s %s%s%s  %s",
		dim, shortAgo(e.Time), reset, cyan, e.Sandbox+"/"+e.Service, reset, eventText(e))
}

// footer says what the keys do, and gives way to anything more urgent.
//
// It only names keys that would do something. The scroll keys were advertised whenever the
// pane had focus, including when its nine events fitted on screen with room to spare - so
// `g top` and `G follow` sat in the footer doing nothing, which teaches a reader that the
// hints are decorative and that the rest of them probably lie too.
func footer(m model, paneRows, cols int) string {
	switch {
	case m.input.active && !m.input.typing:
		return limitChoices(m.input.label, cols)

	case m.input.active:
		// A block for a cursor, so it is obvious the dashboard is waiting on a person rather
		// than on a container.
		head := m.input.label + dim + " — cpu,memory" + reset

		return cyan + "  " + head + "  " +
			truncate(m.input.buffer, max(1, cols-visibleLen(head)-24)) +
			invert + " " + reset + dim + "   ⏎ set   esc cancel" + reset

	case m.confirm != "":
		return red + "  " + m.confirm + reset + "   y / n"

	case m.err != nil:
		return red + "  " + truncate(firstLine(m.err.Error()), cols-4) + reset
	}

	var full, short string

	switch {
	case m.focus == focusPane && maxOffset(m, paneRows) > 0:
		full = "  ↑↓ scroll   ⇞⇟ page   g top   G follow   l switches   ⇥ table   q quit"
		short = "  ↑↓ scroll  g top  G follow  ⇥ table  q quit"

	case m.focus == focusPane:
		// Focused, but there is nothing to scroll: everything is already on screen.
		full = "  all of it is on screen   l switches   ⇥ table   r refresh   q quit"
		short = "  l switches  ⇥ table  q quit"

	default:
		full = "  ↑↓ move   ⏎ wake   s sleep   v sandboxes   a system   l logs   L limit   d remove   q quit"
		short = "  ↑↓ ⏎ wake  s sleep  v sbx  a sys  l logs  q quit"
	}

	// Trimmed rather than wrapped: a footer that wraps steals a line from the table and moves
	// everything on the screen by one.
	if cols < visibleLen(full)+2 {
		full = short
	}

	// A message from the last keypress is worth more than the hints, but not forever: the
	// first version never cleared one, so a single `s` left "asleep" in the footer for the
	// rest of the session and the keys were never mentioned again.
	if m.message != "" && time.Since(m.messageAt) < messageLife {
		return green + "  " + truncate(m.message, cols-4) + reset
	}

	return dim + full + reset
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}

	return s
}

// cols are the computed column widths.
type cols struct{ sandbox, service, cpu, mem, address, meters int }

// Sizes for the per-row meters. Ten cells rather than the sixteen the detail block uses,
// because this one is drawn once per service and the column has to earn its width against the
// address, which is what people actually came for.
const (
	tableBarCells = 10
	limitTextCols = 6

	// meterCell is "[..........] 512m": the bar, a space, and the ceiling right-aligned.
	meterCell = tableBarCells + 2 + 1 + limitTextCols

	// metersWidth is both cells and the gap between them.
	metersWidth = 2*meterCell + 2
)

// rowOverhead is everything in a row that is not one of the sized columns: the selection
// marker, the space after it, the four gaps before the address, and the state column.
const rowOverhead = 1 + 1 + (2 * 4) + 6

// widths sizes the columns to the data, within what the terminal has.
//
// The fixed cost is counted rather than estimated. A guessed constant here is how a long
// branch name pushed the address column past the right edge: the row still fit after
// truncation, so nothing looked broken, and the one column somebody needs in order to connect
// had quietly gone.
func widths(rows []row, total int) cols {
	w := cols{sandbox: 7, service: 7, cpu: 6, mem: 7}

	want := 0

	for _, r := range rows {
		w.sandbox = max(w.sandbox, len(r.Sandbox))
		w.service = max(w.service, len(r.Service))
		want = max(want, len(r.Address))
	}

	// A steady width while addresses come and go - but only where there are any. A sandbox row
	// has no address of its own, so the grouped view was reserving fifteen columns for a column
	// it never filled, taking them from the names beside it.
	if want > 0 {
		want = max(want, len("127.0.0.1:20000"))
	}

	// The address is what somebody came for, so it is paid before the names.
	spare := total - rowOverhead - w.cpu - w.mem

	if budget := spare - w.service - want; w.sandbox > budget {
		w.sandbox = max(8, budget)
	}

	if budget := spare - w.sandbox - want; w.service > budget {
		w.service = max(6, budget)
	}

	left := spare - w.sandbox - w.service - 2

	// A sandbox row has no address of its own, so the grouped view spends none of the room on
	// that column - and the meters, which it does have, get first claim on it instead. Tying
	// them to the address is what made a grouped table show no ceilings at all.
	if want == 0 {
		w.address = 0

		if left >= metersWidth {
			w.meters = metersWidth
		}

		return w
	}

	// Whatever is left is the address. Below a usable width it is dropped rather than shown as
	// three characters and an ellipsis.
	if w.address = left; w.address < 8 {
		w.address = 0
	}

	// The meters are last in line and first to go. They are worth having only on a terminal
	// wide enough that the address is already comfortable, because a ceiling is context and an
	// address is the thing somebody came to copy.
	// The address keeps what it needs and no more; the meters take their fixed width out of
	// what was going to be padding either way.
	if w.address > 0 && w.address-want >= metersWidth+2 {
		w.address = want
		w.meters = metersWidth
	}

	return w
}

func eventText(e history.Record) string {
	switch {
	case e.Event == "woke" && e.DurationMs > 0:
		return fmt.Sprintf("woke in %dms", e.DurationMs)
	case e.Event == "slept":
		return "slept"
	default:
		return e.Event
	}
}

// plural writes a count and its noun, pluralised. It takes the word because the suffix is not
// a property of the number: the first version returned "es" for anything that was not one,
// which is right for "sandbox" and gave "7 servicees" the moment it was used for anything
// else.
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}

	suffix := "s"
	if strings.HasSuffix(word, "x") || strings.HasSuffix(word, "s") {
		suffix = "es"
	}

	return fmt.Sprintf("%d %s%s", n, word, suffix)
}

func pad(left, right string, cols int) string {
	gap := cols - visibleLen(left) - visibleLen(right)
	if gap < 1 {
		gap = 1
	}

	return left + strings.Repeat(" ", gap) + right
}

// truncate cuts a string to n visible columns, ignoring escape sequences.
//
// Counting bytes here would cut in the middle of an escape sequence and leave the rest of the
// terminal painted in whatever colour that was.
// truncateName shortens a name and admits that it did.
//
// A hard cut gives "postgr" and "sbx-ui-p", which do not read as shortened names. They read
// as a corrupted one, or worse as a different sandbox that happens to start the same way -
// and on a narrow terminal every name in the column is a candidate. One column spent on an
// ellipsis is what makes "there is more of this" visible.
//
// Plain text only, unlike truncate: these are names, and a name with an escape sequence in it
// is a problem for somewhere else.
func truncateName(s string, n int) string {
	r := []rune(s)

	switch {
	case n <= 0:
		return ""
	case len(r) <= n:
		return s
	case n == 1:
		return "…"
	}

	return string(r[:n-1]) + "…"
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}

	var (
		b       strings.Builder
		visible int
		inEsc   bool
	)

	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true

			b.WriteRune(r)
		case inEsc:
			b.WriteRune(r)

			if r == 'm' {
				inEsc = false
			}
		default:
			if visible >= n {
				return b.String() + reset
			}

			b.WriteRune(r)

			visible++
		}
	}

	return b.String()
}

func visibleLen(s string) int {
	n, inEsc := 0, false

	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		default:
			n++
		}
	}

	return n
}

// stripColour removes escapes, for the selected row and for tests, which should assert
// against what a reader sees rather than against a palette.
func stripColour(s string) string {
	var b strings.Builder

	inEsc := false

	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		default:
			b.WriteRune(r)
		}
	}

	return b.String()
}
