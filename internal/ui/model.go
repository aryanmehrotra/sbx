package ui

// What the dashboard knows, and how it turns that into a frame.
//
// Deliberately split from everything that touches a terminal or a docker daemon: render is a
// pure function from state to a string. That is what makes it testable - the interesting bugs
// in a table are alignment, truncation and what happens at eight columns wide, and none of
// those need a TTY to find.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// row is one service, flattened for display.
type row struct {
	Sandbox string
	Service string
	Awake   bool
	Address string
	Ref     string

	// CPU is a share of one core as a percentage, so 250 means two and a half cores. Computed
	// from two samples; unknown until there have been two.
	CPU      float64
	CPUKnown bool

	// OnlineCPUs is how many cores the host has, which is the ceiling a share-of-one-core
	// figure is measured against when the service itself has no cap.
	OnlineCPUs int

	MemBytes uint64
	MemKnown bool
}

// pane names what the bottom of the screen is showing. There are two, and switching between
// them changes that pane's contents and nothing else - the layout does not move, because a
// dashboard that rearranges itself costs the reader their bearings every time.
type pane int

const (
	paneEvents pane = iota
	paneLogs
)

// focus is which half of the screen the arrow keys are driving. Two, and Tab moves between
// them: the alternative is arrows that mean different things depending on a mode nobody can
// see, which is how a reader ends up scrolling a log when they meant to pick a sandbox.
type focus int

const (
	focusTable focus = iota
	focusPane
)

// prompt is a line of text being typed.
//
// The dashboard is otherwise driven entirely by single keypresses, and this is the one place a
// value has to be composed rather than chosen. It lives in the footer so that nothing above it
// moves while somebody types - a screen that reflows around an input is one where the row you
// were aiming at is somewhere else by the time you have finished.
type prompt struct {
	active bool

	// typing is false while the offered ceilings are on screen and true once somebody has
	// asked to write their own. Two steps rather than one, because "cpu,memory" is a syntax
	// to remember and most of the time the answer is one of three or four ordinary sizes -
	// and a dashboard that makes you type the ordinary case has got its defaults wrong.
	typing bool

	label  string
	buffer string

	// ref is the service the answer is for, captured when the prompt opened. The selection can
	// move underneath a prompt - the fleet refreshes every second - and a limit applied to
	// whatever happens to be selected on submit is a limit applied to the wrong container.
	ref  string
	name string
}

// serviceStat is what the history says about one service, summarised for the detail line.
type serviceStat struct {
	wakes      int
	lastWakeMs int64
}

// model is the whole state of the screen.
type model struct {
	rows     []row
	selected int
	events   []history.Record

	// stats is keyed by "sandbox/service" and derived from the journal, so the detail line can
	// say how often this thing has actually woken rather than only what it is doing now.
	stats map[string]serviceStat

	// pane is which content the bottom shows; logs holds the selected service's output.
	pane pane
	logs []string

	// focus is what the arrows drive. offset is how far the pane is scrolled back from the
	// end, in lines, so 0 always means "following the tail" - the state people expect after
	// pressing End, and the one a new line should not knock them out of.
	focus  focus
	offset int

	// update is a newer version, or "". Read from a cache, never fetched here.
	update  string
	version string

	// err is the last refresh failure, shown rather than hidden: a dashboard that silently
	// stops updating when docker dies is worse than one that says docker died.
	err error

	// limits is what each service is allowed, by ref.
	//
	// All of them rather than only the selected one, because the table draws a meter per row
	// and a table that could only fill in the highlighted line would be worse than one that
	// filled in none. What makes that affordable is that a ceiling cannot change unless this
	// program changes it or the container is recreated, so they are read rarely - see
	// limitTTL - rather than on every refresh.
	limits map[string]provider.Limits

	// input is a line being typed, if one is.
	input prompt

	// confirm is a pending destructive action.
	confirm string

	// message is transient feedback: what the last key did, and when, so it can fade rather
	// than sit in the footer for the rest of the session.
	message   string
	messageAt time.Time

	provider string
}

// currentRow is the selected row, if there is one.
func (m model) currentRow() (row, bool) {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return row{}, false
	}

	return m.rows[m.selected], true
}

// summarise counts what the journal says about each service, for the detail line.
func summarise(events []history.Record) map[string]serviceStat {
	out := map[string]serviceStat{}

	for _, e := range events {
		if e.Event != "woke" {
			continue
		}

		k := e.Sandbox + "/" + e.Service

		s := out[k]
		s.wakes++

		if e.DurationMs > 0 {
			s.lastWakeMs = e.DurationMs
		}

		out[k] = s
	}

	return out
}

// counts summarises the fleet for the header.
// counts summarises the fleet: how many sandboxes, how many services in them, and how many of
// those services are awake.
//
// Services as well as sandboxes because the first version returned only the two numbers the
// header printed, and they counted different things: "3 sandboxes · 1 awake" was three
// sandboxes and one awake *service*, which reads as one awake sandbox and is not. With four
// services up it said "3 sandboxes · 4 awake", and four out of three is the kind of arithmetic
// a reader has to stop and work out. A service is one container - see spec.Service - so this
// is also the container count.
func (m model) counts() (sandboxes, services, awake int) {
	seen := map[string]bool{}

	for _, r := range m.rows {
		if !seen[r.Sandbox] {
			seen[r.Sandbox] = true
			sandboxes++
		}

		services++

		if r.Awake {
			awake++
		}
	}

	return sandboxes, services, awake
}

// rowsFrom flattens the provider's units into display rows, in a stable order.
//
// Stable matters more than it sounds: the list is redrawn every second, and a provider that
// returns units in map order would make the selection jump under the cursor while somebody is
// reaching for `d`.
func rowsFrom(units []provider.Unit) []row {
	out := make([]row, 0, len(units))

	for _, u := range units {
		out = append(out, row{
			Sandbox: u.Sandbox,
			Service: u.Service,
			Awake:   u.Running,
			Address: addressOf(u),
			Ref:     u.Ref,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Sandbox != out[j].Sandbox {
			return out[i].Sandbox < out[j].Sandbox
		}

		return out[i].Service < out[j].Service
	})

	return out
}

func addressOf(u provider.Unit) string {
	if len(u.Client) == 0 {
		return ""
	}

	parts := make([]string, 0, len(u.Client))
	for _, e := range u.Client {
		parts = append(parts, fmt.Sprintf("%s:%d", e.Host, e.Port))
	}

	return strings.Join(parts, " ")
}

// limitOf is what one service is allowed, or nothing known yet, which reads the same as
// uncapped and is the only honest thing to draw before the answer arrives.
func (m model) limitOf(ref string) provider.Limits { return m.limits[ref] }

// cpuPercent turns two samples into a share of one core.
//
// The arithmetic docker's own CLI uses: the ratio of the container's CPU-time delta to the
// host's, multiplied by the number of cores. A single sample cannot produce this, which is
// why the first frame shows nothing rather than a zero - a zero would be a claim, and it
// would be wrong.
func cpuPercent(prev, cur provider.Usage) (float64, bool) {
	if cur.SystemNanos <= prev.SystemNanos || cur.CPUNanos < prev.CPUNanos {
		// The counters went backwards, which means the container restarted between samples.
		return 0, false
	}

	sys := float64(cur.SystemNanos - prev.SystemNanos)
	if sys == 0 {
		return 0, false
	}

	cpus := cur.OnlineCPUs
	if cpus == 0 {
		cpus = 1
	}

	return float64(cur.CPUNanos-prev.CPUNanos) / sys * float64(cpus) * 100, true
}

// humanBytes is the memory column. Two significant figures is all a dashboard needs, and a
// column of "1.234 GB" is harder to compare at a glance than one of "1.2 GB".
func humanBytes(b uint64) string {
	switch {
	case b == 0:
		return "0"
	case b < 1<<10:
		return fmt.Sprintf("%d B", b)
	case b < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	case b < 1<<30:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	}
}

// shortAgo is a compact "when", for the event pane.
func shortAgo(t time.Time) string {
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
