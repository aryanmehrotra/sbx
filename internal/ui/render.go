package ui

// Turning the model into a frame.
//
// Pure: state in, string out, no terminal involved. Every hard case here - a name longer than
// the column, a terminal eighty columns wide, more sandboxes than rows on screen - is a test
// rather than something found by resizing a window and squinting.

import (
	"fmt"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/history"
)

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
)

// minCols is the narrowest terminal this lays out for. Below it the address column is dropped
// rather than wrapped: a table that wraps is not a table.
const minCols = 60

func render(m model, rows, cols int) string {
	if m.logs != nil {
		return renderLogs(m, rows, cols)
	}

	if cols < 20 || rows < 6 {
		return "terminal too small"
	}

	var b strings.Builder

	lines := 0
	write := func(s string) {
		b.WriteString(truncate(s, cols))
		b.WriteString("\n")
		lines++
	}

	// ── header ───────────────────────────────────────────────────────────────
	sandboxes, awake := m.counts()

	left := fmt.Sprintf("%ssbx%s %s%s%s", bold, reset, dim, m.version, reset)
	right := fmt.Sprintf("%d sandbox%s · %d awake", sandboxes, plural(sandboxes), awake)

	if m.update != "" {
		// The whole point of the notice: visible, and never in the way of the data.
		right = fmt.Sprintf("%s%s available%s · %s", yellow, m.update, reset, right)
	}

	write(pad(left, right, cols))
	write(dim + strings.Repeat("─", cols) + reset)

	// ── table ────────────────────────────────────────────────────────────────
	w := widths(m.rows, cols)

	write(fmt.Sprintf("%s  %-*s  %-*s  %-*s  %*s  %*s  %s%s",
		dim, w.sandbox, "SANDBOX", w.service, "SERVICE", 6, "STATE",
		w.cpu, "CPU", w.mem, "MEMORY", "ADDRESS", reset))

	// Rows left for the table: the header took three lines, and the footer, the event pane
	// and its rules need the rest.
	footer := 4 + min(len(m.events), 3)
	space := rows - lines - footer

	if space < 1 {
		space = 1
	}

	start := 0
	if m.selected >= space {
		start = m.selected - space + 1
	}

	for i := start; i < len(m.rows) && i-start < space; i++ {
		write(renderRow(m.rows[i], i == m.selected, w))
	}

	if len(m.rows) == 0 {
		write("")

		// "no sandboxes yet" is a claim about the fleet, and a failed listing is not evidence
		// for it: nothing was found because nothing could be looked at. Saying both at once -
		// which it did - tells somebody whose docker is down that they have no sandboxes.
		if m.err != nil {
			write(red + "  could not read the fleet - the error is below" + reset)
		} else {
			write(dim + "  no sandboxes yet.  sbx init  makes one." + reset)
		}
	}

	// ── events ───────────────────────────────────────────────────────────────
	for lines < rows-footer {
		write("")
	}

	write(dim + strings.Repeat("─", cols) + reset)

	if len(m.events) == 0 {
		write(dim + "  nothing has happened yet" + reset)
	}

	for _, e := range m.events {
		write(fmt.Sprintf("%s  %-4s%s %s%s/%s%s  %s",
			dim, shortAgo(e.Time), reset, cyan, e.Sandbox, e.Service, reset, eventText(e)))
	}

	// ── footer ───────────────────────────────────────────────────────────────
	write(dim + strings.Repeat("─", cols) + reset)

	switch {
	case m.confirm != "":
		write(fmt.Sprintf("%s  %s%s  y / n", red, m.confirm, reset))
	case m.err != nil:
		write(fmt.Sprintf("%s  %s%s", red, truncate(m.err.Error(), cols-4), reset))
	case m.message != "":
		write(fmt.Sprintf("%s  %s%s", green, m.message, reset))
	default:
		write(dim + "  ↑↓ move   ⏎ wake   s sleep   l logs   d remove   r refresh   q quit" + reset)
	}

	return strings.TrimRight(b.String(), "\n")
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
	// there is nothing running to measure, which is the point of the project.
	cpu, mem := "-", "-"
	if r.Awake {
		if r.CPUKnown {
			cpu = fmt.Sprintf("%.1f%%", r.CPU)
		} else {
			cpu = "…" // one sample so far; a rate needs two
		}

		if r.MemKnown {
			mem = humanBytes(r.MemBytes)
		} else {
			mem = "…"
		}
	}

	line := fmt.Sprintf("%s %-*s  %-*s  %s%-6s%s  %*s  %*s  %s%s%s",
		marker,
		w.sandbox, truncate(r.Sandbox, w.sandbox),
		w.service, truncate(r.Service, w.service),
		colour, state, reset,
		w.cpu, cpu,
		w.mem, mem,
		dim, r.Address, reset)

	if selected {
		return invert + stripColour(line) + reset
	}

	return line
}

func renderLogs(m model, rows, cols int) string {
	var b strings.Builder

	b.WriteString(truncate(bold+"  "+m.logsTitle+reset, cols) + "\n")
	b.WriteString(dim + strings.Repeat("─", cols) + reset + "\n")

	// The last screenful, because the interesting end of a log is the bottom.
	space := rows - 4
	start := max(0, len(m.logs)-space)

	shown := 0

	for _, l := range m.logs[start:] {
		b.WriteString(truncate("  "+l, cols) + "\n")
		shown++
	}

	for ; shown < space; shown++ {
		b.WriteString("\n")
	}

	b.WriteString(dim + strings.Repeat("─", cols) + reset + "\n")
	b.WriteString(dim + "  any key returns" + reset)

	return b.String()
}

// rowOverhead is everything in a row that is not one of the sized columns: the selection
// marker, the space after it, the five gaps, and the state column.
const rowOverhead = 1 + 1 + (2 * 5) + 6

// cols are the computed column widths.
type cols struct{ sandbox, service, cpu, mem int }

// widths sizes the columns to the data, within what the terminal has.
//
// The fixed cost is counted rather than estimated. A guessed constant here is how a long
// branch name pushed the address column past the right edge and off the screen: the row still
// fit after truncation, so nothing looked broken, and the one column somebody needs in order
// to connect had silently gone.
func widths(rows []row, total int) cols {
	w := cols{sandbox: 7, service: 7, cpu: 6, mem: 7}

	addr := len("127.0.0.1:20000")

	for _, r := range rows {
		w.sandbox = max(w.sandbox, len(r.Sandbox))
		w.service = max(w.service, len(r.Service))
		addr = max(addr, len(r.Address))
	}

	// The marker, the space after it, the five two-space gaps between the six columns, and
	// the six-wide state column. rowOverhead is asserted against a real rendered row by a
	// test, because hand-counting a format string is exactly the sort of thing that is wrong
	// by two and produces a table that is quietly two columns too wide.
	const fixed = rowOverhead

	// The address is what somebody came for, so it is paid first and the name column takes
	// whatever is left.
	if budget := total - fixed - w.service - w.cpu - w.mem - addr; w.sandbox > budget {
		w.sandbox = max(8, budget)
	}

	// On a genuinely narrow terminal the service name gives way too, rather than the address.
	if budget := total - fixed - w.sandbox - w.cpu - w.mem - addr; w.service > budget {
		w.service = max(6, budget)
	}

	return w
}

func eventText(e history.Record) string {
	if e.Event == "woke" && e.DurationMs > 0 {
		return fmt.Sprintf("woke in %dms", e.DurationMs)
	}

	if e.Event == "slept" {
		return "slept"
	}

	return e.Event
}

func plural(n int) string {
	if n == 1 {
		return ""
	}

	return "es"
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

// stripColour removes escapes, for the selected row: an inverted line with its own colours
// still in it comes out unreadable on half the terminals that exist.
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
