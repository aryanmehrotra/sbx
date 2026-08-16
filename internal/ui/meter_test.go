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

func TestTheCPUMeterNamesItsDenominator(t *testing.T) {
	awake := row{Awake: true, CPU: 35, CPUKnown: true, OnlineCPUs: 8}

	// Capped at half a core: 0.35 of a core is 70% of what it is allowed.
	capped := plainText(cpuMeter(awake, provider.Limits{NanoCPUs: 500_000_000}))

	if !strings.Contains(capped, "0.35") || !strings.Contains(capped, "0.5 cores") {
		t.Errorf("capped cpu meter = %q, want it to say 0.35 of 0.5 cores", capped)
	}

	// Uncapped: the only honest denominator is the host, and it must say there is no limit
	// rather than quietly measuring against one core.
	free := plainText(cpuMeter(awake, provider.Limits{}))

	if !strings.Contains(free, "8 cores") || !strings.Contains(free, "no limit set") {
		t.Errorf("uncapped cpu meter = %q, want it to name the host's 8 cores and say no "+
			"limit is set", free)
	}
}

// Docker reports an uncapped container's memory limit as the whole host's memory. Passing
// that on as a denominator says a redis holding 3 MB of a laptop is "0.04% full", which is a
// true statement about a limit that does not exist.
func TestTheHostsMemoryIsNotALimit(t *testing.T) {
	r := row{Awake: true, MemBytes: 3 << 20, MemKnown: true}

	got := plainText(memMeter(r, provider.Limits{}))

	if !strings.Contains(got, "no limit set") {
		t.Errorf("uncapped memory meter = %q, want it to say no limit is set", got)
	}

	if strings.Contains(got, "%") {
		t.Errorf("uncapped memory meter = %q, want no proportion of a limit that is not "+
			"there", got)
	}

	capped := plainText(memMeter(r, provider.Limits{MemBytes: 256 << 20}))

	if !strings.Contains(capped, "3 MB") || !strings.Contains(capped, "256 MB") {
		t.Errorf("capped memory meter = %q, want 3 MB of 256 MB", capped)
	}
}

// A service that has not been sampled yet must say so. A zero would be a claim, and it would
// be wrong.
func TestAnUnsampledServiceClaimsNothing(t *testing.T) {
	for name, got := range map[string]string{
		"cpu":    plainText(cpuMeter(row{Awake: true}, provider.Limits{NanoCPUs: 1e9})),
		"memory": plainText(memMeter(row{Awake: true}, provider.Limits{MemBytes: 1 << 20})),
	} {
		if !strings.Contains(got, "not sampled") {
			t.Errorf("%s meter with no sample = %q, want it to say so rather than show 0",
				name, got)
		}
	}
}

// The bar is a proportion, and the two ends are where a rounding mistake shows.
func TestTheBarsEnds(t *testing.T) {
	if full := plainText(bar(1)); strings.Contains(full, "·") {
		t.Errorf("a full bar still has empty cells: %q", full)
	}

	if over := plainText(bar(4)); len(strings.Split(over, "█")) > barCells+1 {
		t.Errorf("a bar past its limit overflowed its width: %q", over)
	}

	// Nothing at all is empty; a very little is not, because "barely using it" and "switched
	// off" are different things and an empty bar says the second.
	if none := plainText(bar(0)); strings.Contains(none, "█") {
		t.Errorf("a bar at zero drew a filled cell: %q", none)
	}

	if tiny := plainText(bar(0.001)); !strings.Contains(tiny, "█") {
		t.Errorf("a bar at 0.1%% drew nothing, which reads as switched off: %q", tiny)
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

// A sleeping service has no usage, so the layout must not reserve the meters' two lines for
// it - that is two blank lines under the sandbox, which is the padding this dashboard was
// built to avoid.
func TestASleepingServiceDoesNotReserveMeterLines(t *testing.T) {
	asleep := model{rows: []row{{Sandbox: "a", Service: "one"}}}
	awake := model{rows: []row{{Sandbox: "a", Service: "one", Awake: true}}}

	if got := wantDetail(asleep); got != detailNoMeters {
		t.Errorf("a sleeping service asked for %d detail lines, want %d", got, detailNoMeters)
	}

	if got := wantDetail(awake); got != detailFull {
		t.Errorf("an awake service asked for %d detail lines, want %d", got, detailFull)
	}

	// And the block it actually renders has no blank line in it.
	block := detailBlock(asleep, plan(30, 1, wantDetail(asleep)).detailRows, 100)
	for i, l := range block {
		if strings.TrimSpace(plainText(l)) == "" {
			t.Errorf("line %d of a sleeping service's detail block is blank: %q", i, l)
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
