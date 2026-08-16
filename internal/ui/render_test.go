package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// plain is a frame with the escape sequences taken out, which is what the reader actually
// sees and the only sane thing to make assertions about.
func plain(s string) string { return stripColour(s) }

func sample() model {
	return model{
		version: "v0.1.0",
		rows: []row{
			{Sandbox: "feature-x", Service: "postgres", Awake: true, Address: "127.0.0.1:20040",
				CPU: 5.5, CPUKnown: true, MemBytes: 28 << 20, MemKnown: true},
			{Sandbox: "feature-x", Service: "redis", Awake: false, Address: "127.0.0.1:20041"},
			{Sandbox: "main", Service: "postgres", Awake: false, Address: "127.0.0.1:20002"},
		},
	}
}

// Every line must fit. A frame with a line wider than the terminal wraps, and a wrapped line
// pushes everything below it down by one - so the bottom of the dashboard walks off the
// screen and the footer disappears.
func TestNoLineIsWiderThanTheTerminal(t *testing.T) {
	for _, cols := range []int{60, 80, 100, 120, 200} {
		for _, rows := range []int{10, 24, 40} {
			frame := render(sample(), rows, cols)

			for i, line := range strings.Split(frame, "\n") {
				if n := visibleLen(line); n > cols {
					t.Errorf("at %dx%d line %d is %d columns wide:\n%q", cols, rows, i, n, plain(line))
				}
			}
		}
	}
}

// A frame taller than the terminal scrolls the top away, which on a redraw-in-place dashboard
// means the header vanishes and never comes back.
func TestTheFrameFitsTheRowsItWasGiven(t *testing.T) {
	for _, rows := range []int{8, 12, 24, 50} {
		frame := render(sample(), rows, 100)

		if got := len(strings.Split(frame, "\n")); got > rows {
			t.Errorf("asked for %d rows, produced %d", rows, got)
		}
	}
}

// The claim this project is built on has to be visible: an asleep service costs nothing, and
// showing 0.0% / 0 B would read as a measurement of something that is not running.
func TestAnAsleepServiceShowsNoUsageRatherThanZero(t *testing.T) {
	frame := plain(render(sample(), 24, 100))

	var redis string

	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "redis") {
			redis = line
		}
	}

	if redis == "" {
		t.Fatal("the redis row is missing entirely")
	}

	if strings.Contains(redis, "0.0%") || strings.Contains(redis, "0 B") {
		t.Errorf("an asleep service is reported as measured zero rather than as not running:\n%s", redis)
	}

	if !strings.Contains(redis, "asleep") {
		t.Errorf("the redis row does not say it is asleep:\n%s", redis)
	}
}

// A rate needs two samples. Showing 0.0%% off the first one is a claim that the service is
// idle, which is not something one sample can know.
func TestCPUIsNotClaimedFromASingleSample(t *testing.T) {
	m := sample()
	m.rows[0].CPUKnown = false

	frame := plain(render(m, 24, 100))

	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "postgres") && strings.Contains(line, "AWAKE") {
			if strings.Contains(line, "0.0%") {
				t.Errorf("cpu was reported as 0.0%% from one sample:\n%s", line)
			}

			return
		}
	}

	t.Fatal("no awake postgres row found")
}

func TestTheUpdateNoticeAppearsOnlyWhenThereIsOne(t *testing.T) {
	m := sample()

	if strings.Contains(plain(render(m, 24, 100)), "available") {
		t.Error("an update notice appeared with no update")
	}

	m.update = "v0.2.0"

	frame := plain(render(m, 24, 100))

	if !strings.Contains(frame, "v0.2.0 available") {
		t.Errorf("the update notice is missing:\n%s", strings.Split(frame, "\n")[0])
	}

	// And it must not have pushed the counts off the header.
	if !strings.Contains(frame, "awake") {
		t.Error("the update notice displaced the sandbox counts")
	}
}

// The selected row is inverted across the whole width. A highlight that stops where the text
// ends reads as a rendering fault rather than as a selection, and it was the first thing the
// painted background broke.
func TestTheSelectedRowIsHighlightedAcrossTheWholeWidth(t *testing.T) {
	m := sample()
	m.selected = 0

	const cols = 100

	for _, line := range strings.Split(render(m, 24, cols), "\n") {
		if !strings.Contains(line, invert) {
			continue
		}

		body := line[strings.Index(line, invert)+len(invert):]

		// Trailing escapes are the frame closing itself off; what matters is that none appear
		// inside the row, where they would break the highlight into pieces.
		for {
			trimmed := strings.TrimSuffix(strings.TrimSuffix(body, reset), background)
			if trimmed == body {
				break
			}
			body = trimmed
		}

		if strings.Contains(body, "\x1b[") {
			t.Errorf("the inverted row still carries its own colours: %q", body)
		}

		if n := visibleLen(body); n != cols {
			t.Errorf("the highlight covers %d of %d columns, so it stops mid-row", n, cols)
		}

		return
	}

	t.Fatal("no row was drawn as selected")
}

// Truncation has to count what the reader sees, not bytes. Cutting inside an escape sequence
// leaves the rest of the terminal painted whatever colour that was.
func TestTruncationCountsVisibleColumnsAndClosesColour(t *testing.T) {
	s := green + "hello world" + reset

	got := truncate(s, 5)

	if visibleLen(got) != 5 {
		t.Errorf("truncate to 5 gave %d visible columns: %q", visibleLen(got), got)
	}

	if plain(got) != "hello" {
		t.Errorf("truncate cut the wrong text: %q", plain(got))
	}

	if !strings.HasSuffix(got, reset) {
		t.Errorf("truncated text does not close its colour, so the rest of the terminal keeps "+
			"it: %q", got)
	}
}

// A long name must not push the address column off the screen.
func TestALongSandboxNameDoesNotEatTheTable(t *testing.T) {
	m := sample()
	m.rows[0].Sandbox = strings.Repeat("a-very-long-branch-name", 4)

	frame := render(m, 24, 100)

	for _, line := range strings.Split(frame, "\n") {
		if n := visibleLen(line); n > 100 {
			t.Fatalf("a long name produced a %d column line: %q", n, plain(line))
		}
	}

	if !strings.Contains(plain(frame), "20041") {
		t.Error("the address column was pushed off the screen by a long name")
	}
}

func TestAnEmptyFleetSaysWhatToDo(t *testing.T) {
	frame := plain(render(model{version: "v0.1.0"}, 24, 100))

	if !strings.Contains(frame, "sbx init") {
		t.Errorf("with no sandboxes it does not say how to make one:\n%s", frame)
	}
}

// A dashboard that silently stops updating when docker dies is worse than one that says so.
func TestARefreshFailureIsShown(t *testing.T) {
	m := sample()
	m.err = errFake{"docker is not running"}

	if !strings.Contains(plain(render(m, 24, 100)), "docker is not running") {
		t.Error("a refresh error is not shown anywhere")
	}
}

type errFake struct{ s string }

func (e errFake) Error() string { return e.s }

func TestCPUPercent(t *testing.T) {
	// Two samples: the container used 0.5s of CPU while the host used 4s across 4 cores.
	// 0.5/4 * 4 = 50% of one core.
	prev := provider.Usage{CPUNanos: 1_000_000_000, SystemNanos: 100_000_000_000, OnlineCPUs: 4}
	cur := provider.Usage{CPUNanos: 1_500_000_000, SystemNanos: 104_000_000_000, OnlineCPUs: 4}

	got, ok := cpuPercent(prev, cur)
	if !ok {
		t.Fatal("two good samples produced no rate")
	}

	if got < 49.9 || got > 50.1 {
		t.Errorf("cpuPercent = %.2f, want 50", got)
	}

	// A restart resets the counters. Reporting a huge negative or a wild number is worse than
	// reporting nothing for one frame.
	if _, ok := cpuPercent(cur, prev); ok {
		t.Error("counters going backwards produced a rate rather than nothing")
	}

	// Two identical samples cannot produce a rate either.
	if _, ok := cpuPercent(cur, cur); ok {
		t.Error("a zero interval produced a rate")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		0:        "0",
		512:      "512 B",
		2048:     "2 KB",
		28 << 20: "28 MB",
		3 << 30:  "3.0 GB",
	}

	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// The rows are redrawn every second. If their order is not stable the selection moves under
// the cursor while somebody is reaching for the remove key.
func TestRowOrderIsStable(t *testing.T) {
	units := []provider.Unit{
		{Sandbox: "b", Service: "redis"},
		{Sandbox: "a", Service: "redis"},
		{Sandbox: "a", Service: "postgres"},
	}

	want := []string{"a/postgres", "a/redis", "b/redis"}

	for range 5 {
		got := rowsFrom(units)

		for i, r := range got {
			if r.Sandbox+"/"+r.Service != want[i] {
				t.Fatalf("row %d is %s/%s, want %s", i, r.Sandbox, r.Service, want[i])
			}
		}
	}
}

func TestEventsAreShown(t *testing.T) {
	m := sample()
	m.events = []history.Record{
		{Time: time.Now().Add(-8 * time.Second), Sandbox: "feature-x", Service: "redis", Event: "slept"},
		{Time: time.Now().Add(-2 * time.Second), Sandbox: "feature-x", Service: "redis", Event: "woke", DurationMs: 191},
	}

	frame := plain(render(m, 24, 100))

	if !strings.Contains(frame, "woke in 191ms") {
		t.Errorf("the wake event and its duration are not shown:\n%s", frame)
	}

	if !strings.Contains(frame, "slept") {
		t.Error("the sleep event is not shown")
	}
}

// rowOverhead is a hand-counted constant describing a format string, which is the kind of
// thing that is silently wrong by two. This checks it against rows that were actually
// rendered, so changing the layout without changing the constant fails here rather than by
// pushing the address column off somebody's screen.
func TestRowOverheadMatchesTheLayout(t *testing.T) {
	r := row{Sandbox: "abcdefghij", Service: "abcdefgh", Awake: true, Address: "127.0.0.1:20000",
		CPU: 1, CPUKnown: true, MemBytes: 1 << 20, MemKnown: true}

	t.Run("without an address column", func(t *testing.T) {
		w := cols{sandbox: 10, service: 8, cpu: 6, mem: 7}

		got := visibleLen(renderRow(r, false, w))
		want := rowOverhead + w.sandbox + w.service + w.cpu + w.mem

		if got != want {
			t.Errorf("a rendered row is %d columns but the budget assumes %d - rowOverhead is "+
				"%d and does not match the format string", got, want, rowOverhead)
		}
	})

	t.Run("with one", func(t *testing.T) {
		w := cols{sandbox: 10, service: 8, cpu: 6, mem: 7, address: len(r.Address)}

		got := visibleLen(renderRow(r, false, w))

		// The two extra columns are the gap before the address, which widths() budgets for
		// separately when it works out what is left over.
		want := rowOverhead + w.sandbox + w.service + w.cpu + w.mem + 2 + w.address

		if got != want {
			t.Errorf("a row with an address is %d columns, budget assumes %d", got, want)
		}
	})
}

// A sample that did not arrive is not a measurement of zero. An awake service whose stats
// call failed showed "0" in the memory column, which reads as "this database is using no
// memory" rather than "sbx did not get an answer".
func TestAnAwakeServiceWithNoSampleDoesNotClaimZero(t *testing.T) {
	m := sample()
	m.rows[0].MemKnown = false
	m.rows[0].MemBytes = 0

	for _, line := range strings.Split(plain(render(m, 24, 100)), "\n") {
		if strings.Contains(line, "postgres") && strings.Contains(line, "AWAKE") {
			fields := strings.Fields(line)
			for _, f := range fields {
				if f == "0" {
					t.Errorf("an awake service with no sample reports 0 bytes:\n%s", line)
				}
			}

			return
		}
	}

	t.Fatal("no awake row found")
}

// A failed listing is not evidence that there are no sandboxes. Showing the empty-state hint
// next to a docker error tells somebody whose daemon is down that their fleet is empty.
func TestAFailedListingIsNotAnEmptyFleet(t *testing.T) {
	m := model{version: "v0.1.0", err: errFake{"dial unix /var/run/docker.sock: no such file"}}

	frame := plain(render(m, 24, 100))

	if strings.Contains(frame, "no sandboxes yet") {
		t.Errorf("it reports an empty fleet while also reporting it could not look:\n%s", frame)
	}

	if !strings.Contains(frame, "could not read the fleet") {
		t.Errorf("it does not say the listing failed:\n%s", frame)
	}

	// And with no error, the hint is exactly what should appear.
	if !strings.Contains(plain(render(model{version: "v0.1.0"}, 24, 100)), "no sandboxes yet") {
		t.Error("a genuinely empty fleet lost its hint")
	}
}
