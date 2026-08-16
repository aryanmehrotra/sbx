package ui

// What a usage figure means against what the service is allowed.
//
// The bug these guard is not a crash. It is a number that reads as one thing and means
// another: "86.8%" is a share of one core, which on an eight-core host is about a ninth of the
// machine - unless the service is capped at one core, in which case it is nearly full. Same
// number, opposite readings, and nothing on screen to tell them apart.

import (
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

func TestTheCPULineNamesItsDenominator(t *testing.T) {
	m := model{metered: true, rows: []row{
		{Sandbox: "a", Service: "b", Ref: "r1", Awake: true, CPU: 35, CPUKnown: true, OnlineCPUs: 8},
	}}

	// Capped at half a core: 0.35 of a core is 70% of what it is allowed.
	m.limits = map[string]provider.Limits{"r1": {NanoCPUs: 500_000_000}}

	capped := plainText(trend(m, m.rows[0], true, 20))

	if !strings.Contains(capped, "0.35/0.5c") {
		t.Errorf("capped cpu line = %q, want it to say 0.35 of 0.5 cores", capped)
	}

	// Uncapped: the figure stands alone rather than being measured against a limit that is
	// not there.
	m.limits = nil

	free := plainText(trend(m, m.rows[0], true, 20))

	if strings.Contains(free, "/") {
		t.Errorf("uncapped cpu line = %q, want no denominator at all", free)
	}
}

// Docker reports an uncapped container's memory limit as the whole host's memory. Passing
// that on as a denominator says a redis holding 3 MB of a laptop is "0.04% full", which is a
// true statement about a limit that does not exist.
func TestTheHostsMemoryIsNotALimit(t *testing.T) {
	m := model{metered: true, rows: []row{
		{Sandbox: "a", Service: "b", Ref: "r1", Awake: true, MemBytes: 3 << 20, MemKnown: true},
	}}

	got := plainText(trend(m, m.rows[0], false, 20))

	if strings.Contains(got, "/") {
		t.Errorf("uncapped memory line = %q, want no proportion of a limit that is not there", got)
	}

	m.limits = map[string]provider.Limits{"r1": {MemBytes: 256 << 20}}

	capped := plainText(trend(m, m.rows[0], false, 20))

	if !strings.Contains(capped, "3m/256m") {
		t.Errorf("capped memory line = %q, want 3m of 256m", capped)
	}
}

// A service that has not been sampled yet must say so. A zero would be a claim, and it would
// be wrong.
func TestAnUnsampledServiceClaimsNothing(t *testing.T) {
	m := model{metered: true, rows: []row{{Sandbox: "a", Service: "b", Ref: "r1", Awake: true}}}
	m.limits = map[string]provider.Limits{"r1": {NanoCPUs: 1e9, MemBytes: 1 << 20}}

	for _, isCPU := range []bool{true, false} {
		got := plainText(trend(m, m.rows[0], isCPU, 20))

		if strings.Contains(got, "0.00") || strings.Contains(got, "0m") {
			t.Errorf("an unsampled service was reported as measured zero: %q", got)
		}
	}
}

// A backend with no metrics has to say so rather than look like it is still loading.
func TestATrendOnAnUnmeteredBackendSaysSo(t *testing.T) {
	m := model{rows: []row{{Sandbox: "a", Service: "b", Ref: "r1", Awake: true}}}

	if got := plainText(trend(m, m.rows[0], true, 20)); !strings.Contains(got, "does not report") {
		t.Errorf("trend on an unmetered backend = %q, want it to say so", got)
	}
}

// The graph is drawn from samples this dashboard took, and before it has any it must say that
// rather than draw a flat line that looks like a service doing nothing.
func TestTheGraphWithNoReadings(t *testing.T) {
	if got := plainText(spark(nil, 0, 20)); !strings.Contains(got, "no readings") {
		t.Errorf("spark with no samples = %q, want it to say there are none", got)
	}
}

// litRows reports which of a braille cell's four rows have a dot in them, top first. The
// graph's whole claim is that height means something, so the tests read the height back.
func litRows(s string) map[int]bool {
	out := map[int]bool{}

	for _, r := range s {
		if r < 0x2800 || r > 0x28ff {
			continue
		}

		bits := r - 0x2800

		for row := range 4 {
			if bits&brailleDots[row][0] != 0 || bits&brailleDots[row][1] != 0 {
				out[row] = true
			}
		}
	}

	return out
}

// Scaled to the ceiling when there is one, so a line near the top means near the limit and
// two services with the same limit are directly comparable.
func TestTheGraphScalesToTheCeiling(t *testing.T) {
	full := litRows(spark([]float64{1, 1, 1, 1}, 1, 10))

	if !full[0] {
		t.Error("a series sitting at its ceiling was not drawn at the top of the cell")
	}

	if full[3] {
		t.Error("a series sitting at its ceiling also lit the bottom row")
	}

	tenth := litRows(spark([]float64{0.1, 0.1, 0.1, 0.1}, 1, 10))

	if !tenth[3] {
		t.Error("a series at a tenth of its ceiling was not drawn at the bottom")
	}

	if tenth[0] {
		t.Error("a series at a tenth of its ceiling reached the top row")
	}

	// And it never draws more cells than the width it was given.
	long := make([]float64, 500)
	if got := len([]rune(plainText(spark(long, 1, 12)))); got > 12 {
		t.Errorf("spark drew %d cells into a width of 12", got)
	}
}

// A trace is a line, not a bar chart: a reading does not fill everything beneath it, or a
// service holding steady near its ceiling looks identical to one that climbed there.
func TestTheGraphDrawsALineNotAWall(t *testing.T) {
	rows := litRows(spark([]float64{1, 1, 1, 1}, 1, 10))

	if rows[1] || rows[2] || rows[3] {
		t.Errorf("a flat series at its ceiling filled the rows below it: %v", rows)
	}
}

// A jump has to stay joined, or two unrelated dots read as noise rather than as a line that
// moved.
func TestTheLineJoinsAcrossAJump(t *testing.T) {
	rows := litRows(spark([]float64{0, 1}, 1, 10))

	for r := range 4 {
		if !rows[r] {
			t.Errorf("a jump from empty to full left row %d unlit, so the line is broken: %v",
				r, rows)
		}
	}
}

// The header's two numbers used to count different things: sandboxes, and awake services. It
// printed "3 sandboxes · 4 awake" over a table of seven services, and four out of three is
// arithmetic a reader has to stop and work out.
func TestTheHeaderCountsOneThing(t *testing.T) {
	m := model{rows: []row{
		{Sandbox: "a", Service: "one", Awake: true},
		{Sandbox: "b", Service: "two", Awake: true},
		{Sandbox: "b", Service: "three"},
	}}

	sandboxes, services, awake := m.counts()

	if sandboxes != 2 || services != 3 || awake != 2 {
		t.Fatalf("counts() = %d sandboxes, %d services, %d awake; want 2, 3, 2",
			sandboxes, services, awake)
	}

	got := plainText(title(m, 120))

	if !strings.Contains(got, "2 sandboxes") || !strings.Contains(got, "2 of 3 services awake") {
		t.Errorf("title = %q, want it to say 2 sandboxes and 2 of 3 services awake", got)
	}
}

// One sandbox and one service must not be told they are plural, and "service" must not take
// the "es" that "sandbox" does.
func TestPluralsOfBothWords(t *testing.T) {
	for _, c := range []struct {
		n          int
		word, want string
	}{
		{1, "sandbox", "1 sandbox"},
		{2, "sandbox", "2 sandboxes"},
		{1, "service", "1 service"},
		{7, "service", "7 services"},
	} {
		if got := plural(c.n, c.word); got != c.want {
			t.Errorf("plural(%d, %q) = %q, want %q", c.n, c.word, got, c.want)
		}
	}
}

// Every line of the block is worth drawing for a sleeping service too - its usage line shows
// the drop to zero and when it happened - so the block must never be padded with blanks.
func TestASleepingServiceFillsItsBlock(t *testing.T) {
	asleep := model{
		rows:    []row{{Sandbox: "a", Service: "one", Ref: "r1", Address: "127.0.0.1:20000"}},
		metered: true,
	}

	block := detailBlock(asleep, plan(30, 1, wantDetail(asleep)).detailRows, 120)

	for i, l := range block {
		if strings.TrimSpace(plainText(l)) == "" {
			t.Errorf("line %d of a sleeping service's detail block is blank: %q", i, l)
		}
	}
}

// A sleeping database raises exactly one question, and the table's STATE column does not
// answer it. The old block said "volume intact" in a sentence of its own; compacting the
// block must not lose the reassurance with the line.
func TestASleepingServiceStillSaysTheVolumeIsIntact(t *testing.T) {
	m := model{
		rows:    []row{{Sandbox: "a", Service: "db", Ref: "r1", Address: "127.0.0.1:20000"}},
		metered: true,
	}

	got := plainText(strings.Join(detailBlock(m, detailFull, 140), "\n"))

	if !strings.Contains(got, "volume intact") {
		t.Errorf("a sleeping service's block never says the volume survives:\n%s", got)
	}
}

// A name that has been shortened has to look shortened. "postgr" and "sbx-ui-p" read as a
// corrupted name, or as a different sandbox that happens to start the same way.
func TestAShortenedNameSaysSo(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"postgres", 6, "postg…"},
		{"sbx-ui-polish", 8, "sbx-ui-…"},
		{"redis", 6, "redis"}, // fits, so it is left alone
		{"redis", 5, "redis"}, // exactly fits
		{"redis", 1, "…"},
		{"redis", 0, ""},
	} {
		if got := truncateName(c.in, c.n); got != c.want {
			t.Errorf("truncateName(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}

	// And whatever it returns still fits the column it was given.
	for n := 1; n <= 12; n++ {
		if got := len([]rune(truncateName("a-very-long-sandbox-name", n))); got > n {
			t.Errorf("truncateName at width %d returned %d columns", n, got)
		}
	}
}

// The per-row meters are the last thing in line for width and the first to go, because a
// ceiling is context and an address is the thing somebody came to copy.
func TestTheMetersOnlyTakeSpareWidth(t *testing.T) {
	rows := []row{
		{Sandbox: "zn-dev", Service: "clickhouse", Awake: true,
			Address: "127.0.0.1:20000 127.0.0.1:20001"},
		{Sandbox: "zn-dev", Service: "redis", Address: "127.0.0.1:20003"},
	}

	for _, c := range []struct {
		cols int
		want bool
	}{
		{80, false},
		{118, false},
		{200, true},
	} {
		w := widths(rows, c.cols)

		if got := w.meters > 0; got != c.want {
			t.Errorf("at %d columns meters=%d, want present=%v", c.cols, w.meters, c.want)
		}

		// Whatever it decided, the address must still fit its longest value.
		if w.meters > 0 && w.address < len(rows[0].Address) {
			t.Errorf("at %d columns the meters took the address's room: address=%d, needs %d",
				c.cols, w.address, len(rows[0].Address))
		}
	}
}

// A row must never be wider than the terminal, meters or not.
func TestARowWithMetersStillFits(t *testing.T) {
	m := model{rows: []row{
		{Sandbox: "zn-dev", Service: "clickhouse", Awake: true, Ref: "r1",
			Address: "127.0.0.1:20000 127.0.0.1:20001",
			CPU:     54.5, CPUKnown: true, MemBytes: 467 << 20, MemKnown: true},
		{Sandbox: "two", Service: "sleeper", Ref: "r2", Address: "127.0.0.1:20003"},
	}}

	m.limits = map[string]provider.Limits{
		"r1": {NanoCPUs: 500_000_000, MemBytes: 512 << 20},
	}

	for _, cols := range []int{80, 118, 160, 200, 300} {
		for _, line := range strings.Split(render(m, 24, cols), "\n") {
			if n := visibleLen(line); n > cols {
				t.Fatalf("at %d columns a line is %d wide: %q", cols, n, plainText(line))
			}
		}
	}
}

// An uncapped service gets a dash, not an empty bar: an empty bar is a proportion of nothing,
// drawn as though it meant something.
func TestAnUncappedRowDrawsNoBar(t *testing.T) {
	got := plainText(cell(true, 0.3, 0, "—"))

	if strings.Contains(got, "[") {
		t.Errorf("an uncapped cell drew a bar: %q", got)
	}

	if !strings.Contains(got, "—") {
		t.Errorf("an uncapped cell = %q, want a dash", got)
	}
}

func TestShortBytesReadsLikeSomebodyWouldSayIt(t *testing.T) {
	for _, c := range []struct {
		in   uint64
		want string
	}{
		{512 << 20, "512m"},
		{4 << 30, "4g"},
		{1536 << 20, "1.5g"},
		{0, "—"},
	} {
		if got := shortBytes(c.in); got != c.want {
			t.Errorf("shortBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// plainText strips the escape sequences so a test can assert about what a reader sees.
func plainText(s string) string {
	var (
		b  strings.Builder
		in bool
	)

	for _, r := range s {
		switch {
		case r == '\x1b':
			in = true
		case in && (r == 'm' || r == 'K'):
			in = false
		case !in:
			b.WriteRune(r)
		}
	}

	return b.String()
}

// The detail block's trend lines carry a reading, a percentage, a trace and a legend, and the
// arithmetic that divides the row between them has to add up. It did not: the percentage
// column was left out of the graph's budget, the row overran by five columns, and the
// line-level truncate took the span off the end of the legend - the one number that says how
// far back the graph reaches.
func TestATrendLineFitsAndKeepsItsLegend(t *testing.T) {
	m := model{metered: true, rows: []row{{
		Sandbox: "zn-dev", Service: "clickhouse", Ref: "r1", Awake: true,
		Address: "127.0.0.1:20000 127.0.0.1:20001",
		CPU:     24, CPUKnown: true, MemBytes: 473 << 20, MemKnown: true,
	}}}

	m.limits = map[string]provider.Limits{"r1": {NanoCPUs: 500_000_000, MemBytes: 512 << 20}}

	for _, s := range make([]metricSample, 60) {
		_ = s

		m.series = map[string][]metricSample{"r1": append(m.series["r1"],
			metricSample{cores: 0.24, mem: 473 << 20, known: true})}
	}

	for _, cols := range []int{100, 120, 150, 190, 240} {
		lines := detailBlock(m, detailFull, cols)

		for _, l := range lines {
			if n := visibleLen(l); n > cols {
				t.Errorf("at %d columns a detail line is %d wide: %q", cols, n, plainText(l))
			}
		}

		joined := plainText(strings.Join(lines, "\n"))

		if !strings.Contains(joined, "peak") {
			t.Errorf("at %d columns the legend is missing entirely", cols)
		}

		// The span is the last thing on the legend and so the first thing a too-wide row loses.
		if !strings.Contains(joined, "·") {
			t.Errorf("at %d columns the legend lost its span:\n%s", cols, joined)
		}
	}
}

// Every cell takes the colour of what was happening when it was drawn. One colour for the
// whole line says "fine" over a trace that spent half its length against the ceiling, and when
// it went bad is the one thing a history is for.
func TestTheTraceIsColouredWhereItHappened(t *testing.T) {
	// Low for the first half, pinned against the ceiling for the second.
	var values []float64

	for range 20 {
		values = append(values, 0.1)
	}

	for range 20 {
		values = append(values, 1.0)
	}

	got := spark(values, 1, 40)

	if !strings.Contains(got, green) {
		t.Error("the calm half of the trace was not drawn green")
	}

	if !strings.Contains(got, red) {
		t.Error("the half spent against the ceiling was not drawn red")
	}

	// And the calm half must come first, so the colour follows the data rather than the
	// current reading being smeared backwards over history.
	if strings.Index(got, green) > strings.Index(got, red) {
		t.Error("the trace is coloured by its latest reading rather than by each moment")
	}
}

// A trace with no ceiling has nothing to be alarmed about, so it stays one colour: a
// proportion of nothing is not a warning.
func TestAnUncappedTraceIsNotColouredAsAWarning(t *testing.T) {
	got := spark([]float64{1, 5, 100, 900}, 0, 20)

	if strings.Contains(got, red) || strings.Contains(got, yellow) {
		t.Errorf("an uncapped trace was coloured as though it were near a limit: %q", got)
	}
}
