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
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/provider"
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

	l := plan(rows, len(m.rows), wantDetail(m))
	w := widths(m.rows, cols)

	var out []string

	add := func(s string) { out = append(out, paint(truncate(s, cols), cols)) }
	rule := func() { out = append(out, paint(dim+strings.Repeat("─", cols)+reset, cols)) }

	add(title(m, cols))
	rule()

	if l.header {
		add(tableHeader(w))
	}

	out = append(out, painted(tableRows(m, w, l.tableRows, cols), cols)...)

	if l.detailRows > 0 {
		rule()
		out = append(out, painted(detailBlock(m, l.detailRows, cols), cols)...)
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

	if m.provider != "" {
		right += " · " + m.provider
	}

	if m.update != "" {
		right = fmt.Sprintf("%s%s available%s · %s", yellow, m.update, reset, right)
	}

	return pad(left, right+" ", cols)
}

func tableHeader(w cols) string {
	head := fmt.Sprintf("%s  %-*s  %-*s  %-6s  %*s  %*s",
		dim, w.sandbox, "SANDBOX", w.service, "SERVICE", "STATE", w.cpu, "CPU", w.mem, "MEMORY")

	if w.address > 0 {
		head += "  ADDRESS"
	}

	return head + reset
}

// tableRows renders the visible slice of the table, scrolled to keep the selection in view.
func tableRows(m model, w cols, space, cols int) []string {
	var out []string

	if len(m.rows) == 0 {
		empty := dim + "  no sandboxes yet.  sbx init  makes one." + reset

		// A failed listing is not evidence of an empty fleet: nothing was found because
		// nothing could be looked at, and saying both at once tells somebody whose docker is
		// down that they have no sandboxes.
		if m.err != nil {
			empty = red + "  could not read the fleet - see below" + reset
		}

		out = append(out, truncate(empty, cols))
	}

	start := 0
	if m.selected >= space {
		start = m.selected - space + 1
	}

	for i := start; i < len(m.rows) && len(out) < space; i++ {
		line := truncate(renderRow(m.rows[i], i == m.selected, w), cols)

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

func renderRow(r row, selected bool, w cols) string {
	marker := " "
	if selected {
		marker = "›"
	}

	state, colour := "asleep", dim
	if r.Awake {
		state, colour = "AWAKE", green
	}

	// An asleep service shows a dash rather than 0. Zero is a measurement and this is not one:
	// there is nothing running to measure, which is the point of the project. A sample that
	// has not arrived yet is not zero either, and says so differently.
	cpu, mem := "-", "-"

	if r.Awake {
		cpu, mem = "…", "…"

		if r.CPUKnown {
			cpu = fmt.Sprintf("%.1f%%", r.CPU)
		}

		if r.MemKnown {
			mem = humanBytes(r.MemBytes)
		}
	}

	line := fmt.Sprintf("%s %-*s  %-*s  %s%-6s%s  %*s  %*s",
		marker,
		w.sandbox, truncate(r.Sandbox, w.sandbox),
		w.service, truncate(r.Service, w.service),
		colour, state, reset,
		w.cpu, cpu,
		w.mem, mem)

	if w.address > 0 {
		line += "  " + dim + truncate(shortAddress(r.Address, w.address), w.address) + reset
	}

	return line
}

// highlight marks the selected row, across the full width.
//
// Padded before it is inverted, not after: a highlight that stops where the text does looks
// like a rendering fault rather than a selection, and the painted background would otherwise
// fill the rest of the line and cut it short.
//
// The colours are stripped first because an inverted line that still carries its own
// foreground colours is unreadable on a good half of terminals.
func highlight(line string, cols int) string {
	flat := stripColour(line)

	if gap := cols - visibleLen(flat); gap > 0 {
		flat += strings.Repeat(" ", gap)
	}

	return invert + flat + reset
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
func detailBlock(m model, space, cols int) []string {
	r, ok := m.currentRow()
	if !ok {
		return blanks(space)
	}

	head := fmt.Sprintf(" %s%s/%s%s", cyan, r.Sandbox, r.Service, reset)

	right := ""
	if s, ok := m.stats[r.Sandbox+"/"+r.Service]; ok && s.wakes > 0 {
		right = fmt.Sprintf("woke %d× · last %dms ", s.wakes, s.lastWakeMs)
	}

	// One line: the name and the command, because that is what gets typed next.
	if space == 1 {
		line := head + fmt.Sprintf("   %seval \"$(sbx env %s)\"%s", dim, r.Sandbox, reset)

		return []string{truncate(pad(line, dim+right+reset, cols), cols)}
	}

	out := []string{truncate(pad(head, dim+right+reset, cols), cols)}

	field := func(k, v string) {
		out = append(out, truncate(fmt.Sprintf("   %s%-9s%s %s", dim, k, reset, v), cols))
	}

	field("address", r.Address)
	field("connect", fmt.Sprintf("%seval \"$(sbx env %s)\"%s", dim, r.Sandbox, reset))

	// The meters only appear when the block is tall enough to hold them and the service is
	// awake enough to have readings. When they do, the state line stops repeating the memory
	// figure, because it is on the line below in more useful company.
	meters := r.Awake && space >= detailWithMeters

	switch {
	case r.Awake && meters:
		field("state", green+"awake"+reset)
	case r.Awake:
		field("state", green+"awake"+reset+dim+" · using "+reset+humanBytes(r.MemBytes))
	default:
		field("state", dim+"asleep · 0 B of memory, volume intact, wakes on connect"+reset)
	}

	// With room for only one of them it is memory, because exceeding a memory ceiling kills
	// the container and exceeding a CPU one only makes it slower.
	if meters {
		if space >= detailFull {
			field("cpu", cpuMeter(r, m.limits))
		}

		field("memory", memMeter(r, m.limits))
	}

	field("ref", dim+r.Ref+reset)

	for len(out) < space {
		out = append(out, "")
	}

	return out[:space]
}

// barCells is how wide a usage bar is drawn. Sixteen, because its job is "roughly how full"
// - the figure printed beside it is there for people who want exactly, and a bar wide enough
// to answer that question would crowd out the numbers that answer it better.
const barCells = 16

// bar draws a proportion, and colours it by how alarming it is.
//
// Anything at all is at least one cell. A service sitting at half a percent of its ceiling is
// not the same as one that is switched off, and an empty bar says the second.
func bar(frac float64) string {
	if frac < 0 {
		frac = 0
	}

	filled := int(frac * barCells)

	switch {
	case filled < 1 && frac > 0:
		filled = 1
	case filled > barCells:
		filled = barCells
	}

	colour := green

	switch {
	case frac >= 0.9:
		colour = red
	case frac >= 0.75:
		colour = yellow
	}

	return dim + "[" + reset + colour + strings.Repeat("█", filled) + reset +
		dim + strings.Repeat("·", barCells-filled) + "]" + reset
}

// cpuMeter is what the selected service is using against what it is allowed.
//
// Without a ceiling this can only report a share of one core, and it says so in those words.
// "86.8%" on its own is the number that reads as nearly-full and is usually nothing of the
// kind: on an eight-core machine it is about a ninth of the host.
func cpuMeter(r row, l provider.Limits) string {
	if !r.CPUKnown {
		return dim + "not sampled yet" + reset
	}

	cores := r.CPU / 100

	if l.NanoCPUs <= 0 {
		return fmt.Sprintf("%s%.2f%s%s of %d cores · no limit set%s",
			reset, cores, reset, dim, max(1, r.OnlineCPUs), reset)
	}

	allowed := float64(l.NanoCPUs) / 1e9

	return fmt.Sprintf("%s  %s%.2f%s%s of %s cores%s",
		bar(cores/allowed), reset, cores, reset, dim, trimZeros(allowed), reset)
}

// memMeter is the same for memory, where docker does report a ceiling - but reports the
// host's entire memory when nothing was capped, which is why an uncapped service is told
// apart by Limits rather than by that number.
func memMeter(r row, l provider.Limits) string {
	if !r.MemKnown {
		return dim + "not sampled yet" + reset
	}

	if l.MemBytes == 0 {
		return fmt.Sprintf("%s%s%s%s · no limit set%s", reset, humanBytes(r.MemBytes), reset, dim, reset)
	}

	return fmt.Sprintf("%s  %s%s%s%s of %s%s",
		bar(float64(r.MemBytes)/float64(l.MemBytes)), reset, humanBytes(r.MemBytes), reset,
		dim, humanBytes(l.MemBytes), reset)
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

	if m.pane == paneLogs {
		name, total = "LOGS", len(m.logs)

		if r, ok := m.currentRow(); ok {
			name = "LOGS " + r.Sandbox + "/" + r.Service
		}
	}

	if m.focus == focusPane {
		name = "▸ " + name
	}

	right := "l switches · tab focuses"

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

// tail returns the last n of a slice. Generic because it is wanted for two element types, and
// copying it twice is how the two drift apart.
func tail[T any](s []T, n int) []T {
	if n <= 0 || len(s) == 0 {
		return nil
	}

	if len(s) <= n {
		return s
	}

	return s[len(s)-n:]
}

// footer says what the keys do, and gives way to anything more urgent.
//
// It only names keys that would do something. The scroll keys were advertised whenever the
// pane had focus, including when its nine events fitted on screen with room to spare - so
// `g top` and `G follow` sat in the footer doing nothing, which teaches a reader that the
// hints are decorative and that the rest of them probably lie too.
func footer(m model, paneRows, cols int) string {
	switch {
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
		full = "  ↑↓ move   ⏎ wake   s sleep   l logs   d remove   r refresh   ⇥ pane   q quit"
		short = "  ↑↓ ⏎ wake  s sleep  l logs  d rm  q quit"
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
type cols struct{ sandbox, service, cpu, mem, address int }

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

	want := len("127.0.0.1:20000")

	for _, r := range rows {
		w.sandbox = max(w.sandbox, len(r.Sandbox))
		w.service = max(w.service, len(r.Service))
		want = max(want, len(r.Address))
	}

	// The address is what somebody came for, so it is paid before the names.
	spare := total - rowOverhead - w.cpu - w.mem

	if budget := spare - w.service - want; w.sandbox > budget {
		w.sandbox = max(8, budget)
	}

	if budget := spare - w.sandbox - want; w.service > budget {
		w.service = max(6, budget)
	}

	// Whatever is left is the address. Below a usable width it is dropped rather than shown as
	// three characters and an ellipsis.
	if w.address = spare - w.sandbox - w.service - 2; w.address < 8 {
		w.address = 0
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
