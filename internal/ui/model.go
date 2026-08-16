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

	MemBytes uint64
	MemKnown bool
}

// model is the whole state of the screen.
type model struct {
	rows     []row
	selected int
	events   []history.Record

	// update is a newer version, or "". Read from a cache, never fetched here.
	update  string
	version string

	// err is the last refresh failure, shown rather than hidden: a dashboard that silently
	// stops updating when docker dies is worse than one that says docker died.
	err error

	// confirm is a pending destructive action, and the row it applies to.
	confirm string

	// message is transient feedback: what the last key did.
	message string

	// logs is a full-screen overlay when non-nil.
	logs      []string
	logsTitle string

	provider string
}

// counts summarises the fleet for the header.
func (m model) counts() (sandboxes, awake int) {
	seen := map[string]bool{}

	for _, r := range m.rows {
		if !seen[r.Sandbox] {
			seen[r.Sandbox] = true
			sandboxes++
		}

		if r.Awake {
			awake++
		}
	}

	return sandboxes, awake
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
