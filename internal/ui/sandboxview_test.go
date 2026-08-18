package ui

// The sandbox view.
//
// The table is a list of services, and a sandbox with four of them is four lines repeating one
// name with no line for the thing itself - while every command in this program (`sbx env`,
// `sbx rm`, `sbx logs`) names a sandbox. `v` folds the services up.

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/tui"
)

func twoSandboxes() []row {
	return []row{
		{Sandbox: "work", Service: "mysql", Awake: true, Ref: "r1", Address: "127.0.0.1:1",
			CPU: 5, CPUKnown: true, MemBytes: 300 << 20, MemKnown: true},
		{Sandbox: "work", Service: "redis", Awake: true, Ref: "r2", Address: "127.0.0.1:2",
			CPU: 2, CPUKnown: true, MemBytes: 4 << 20, MemKnown: true},
		{Sandbox: "obs", Service: "console", Ref: "r3", Address: "127.0.0.1:3"},
	}
}

func TestGroupingFoldsServicesIntoTheirSandbox(t *testing.T) {
	m := model{rows: twoSandboxes(), grouped: true}

	got := m.view()
	if len(got) != 2 {
		t.Fatalf("three services in two sandboxes folded to %d rows", len(got))
	}

	work := got[0]

	if work.Sandbox != "work" || work.Members != 2 || work.Woken != 2 {
		t.Errorf("work = %+v, want 2 services with both awake", work)
	}

	// A total, because the question a sandbox row answers is what the whole thing is costing.
	if work.CPU != 7 || work.MemBytes != (304<<20) {
		t.Errorf("work totals cpu=%v mem=%v, want 7 and 304 MiB", work.CPU, work.MemBytes)
	}

	// One awake service means the sandbox is costing memory, which is what the colour is for.
	obs := got[1]
	if obs.Woken != 0 || obs.Awake {
		t.Errorf("obs = %+v, want nothing awake", obs)
	}
}

// A sandbox is not simply awake or asleep, and a row standing for four things saying "AWAKE"
// because one of them is would be a fair summary of the cost and a bad one of the state.
func TestASandboxRowSaysHowManyAreUp(t *testing.T) {
	rows := twoSandboxes()
	rows[1].Awake = false // work now has one of two up

	frame := plain(render(model{version: "v0", rows: rows, grouped: true, metered: true}, 20, 120))

	if !strings.Contains(frame, "1/2 up") {
		t.Errorf("a half-woken sandbox does not say so:\n%s", frame)
	}

	if !strings.Contains(frame, "0/1 up") {
		t.Errorf("a sleeping sandbox does not say so:\n%s", frame)
	}
}

// The row is a total, and a total is the one thing that cannot say which service is the
// expensive one - so the block underneath is the trace and the list it folded up.
func TestTheDetailBlockListsWhatTheSandboxHolds(t *testing.T) {
	m := model{version: "v0", rows: twoSandboxes(), grouped: true, metered: true}

	frame := plain(render(m, 30, 120))

	for _, want := range []string{"mysql", "redis", "127.0.0.1:1", "127.0.0.1:2", "sbx env work"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the sandbox detail never mentions %q:\n%s", want, frame)
		}
	}

	// The trace comes before the member list, and where only some of the block fits it is the
	// half that survives: the services are one `v` away, and the sandbox's own history is not
	// anywhere else on the screen.
	short := plain(render(m, 20, 120))

	for _, want := range []string{"cpu", "memory"} {
		if !strings.Contains(short, want) {
			t.Errorf("a short terminal dropped the sandbox's %s trace:\n%s", want, short)
		}
	}
}

// A key pressed on a line standing for two services has to act on two services. Waking nothing
// while appearing to wake a sandbox is the failure that would make this view a lie.
func TestAKeyOnASandboxActsOnEveryServiceInIt(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.model.rows = twoSandboxes()
	d.model.grouped = true
	d.model.selected = 0

	got := d.targets()
	if len(got) != 2 {
		t.Fatalf("a sandbox of two services targeted %d", len(got))
	}

	for i, want := range []string{"mysql", "redis"} {
		if got[i].Service != want {
			t.Errorf("target %d = %q, want %q", i, got[i].Service, want)
		}
	}

	// Ungrouped, the same key is about the one service it is pointing at.
	d.model.grouped = false
	d.model.selected = 1

	if got := d.targets(); len(got) != 1 || got[0].Service != "redis" {
		t.Errorf("a service row targeted %+v, want redis alone", got)
	}
}

// Logs and limits belong to one container, and a sandbox row stands for several. Saying so
// beats opening an empty pane or applying a ceiling to a name that is not a container.
//
// It is also where a deadlock lived: handle() holds the model's lock, and say() takes it, so
// the first version of this hung the dashboard on a keypress. The matrix test finds that as a
// five-minute timeout rather than a failure, which is a bad way to hear about it.
func TestLogsSayTheyArePerService(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.model.rows = twoSandboxes()
	d.model.grouped = true

	done := make(chan struct{})

	go func() {
		d.handle(context.Background(), tui.Key{Rune: 'l', Code: tui.KeyRune})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("l on a sandbox row never returned - the handler is holding a lock it is " +
			"also waiting for")
	}

	m, _ := d.snapshot()

	if !strings.Contains(m.message, "per service") {
		t.Errorf("l said %q, want it to explain that this acts on one service", m.message)
	}

	if m.pane == paneLogs {
		t.Error("l opened the log pane on a row that has no single service")
	}
}

// A ceiling is a property of one container, so a sandbox's is every one of its services capped
// at the value - not the value shared out between them, and not a refusal.
func TestLimitingASandboxCapsEveryServiceInIt(t *testing.T) {
	p := &limiterProvider{}
	d := newDash(p)
	d.model.rows = twoSandboxes()
	d.model.grouped = true
	d.model.selected = 0 // work: mysql and redis

	d.handle(context.Background(), tui.Key{Rune: 'L', Code: tui.KeyRune})

	if !d.model.input.active {
		t.Fatal("L on a sandbox did not open a prompt")
	}

	if got := d.model.input.refs; len(got) != 2 || got[0] != "r1" || got[1] != "r2" {
		t.Errorf("the prompt is for %v, want both of work's services", got)
	}

	// "each", because the number typed caps every service at that value rather than being
	// divided between them, and those are different instructions.
	if !strings.Contains(d.model.input.label, "each service in work") {
		t.Errorf("label = %q, want it to say the value applies to each service",
			d.model.input.label)
	}

	d.handle(context.Background(), tui.Key{Rune: 'c', Code: tui.KeyRune})
	typeInto(t, d, "0.5,256m\r")

	waitFor(t, func() bool { _, _, n := p.taken(); return n == 2 })

	got, _, n := p.taken()

	if n != 2 {
		t.Errorf("the backend was asked %d times, want once per service", n)
	}

	if got.NanoCPUs != 500_000_000 || got.MemBytes != 256<<20 {
		t.Errorf("the ceiling sent was %+v, want half a core and 256 MB", got)
	}
}

// Twelve services and six sandboxes are different numbers of rows, and the selection counts
// rows. Toggling with the last one selected must not leave it pointing past the end.
func TestTogglingKeepsTheSelectionInside(t *testing.T) {
	d := newDash(&fakeProvider{})
	d.model.rows = twoSandboxes()
	d.model.selected = 2 // the third service; there are only two sandboxes

	d.handle(context.Background(), tui.Key{Rune: 'v', Code: tui.KeyRune})

	m, _ := d.snapshot()

	if !m.grouped {
		t.Fatal("v did not group")
	}

	if m.selected >= len(m.view()) {
		t.Errorf("selection %d is past the end of %d sandboxes", m.selected, len(m.view()))
	}

	// And back, without leaving it somewhere impossible either.
	d.handle(context.Background(), tui.Key{Rune: 'v', Code: tui.KeyRune})

	m, _ = d.snapshot()

	if m.grouped {
		t.Fatal("v did not ungroup")
	}

	if m.selected >= len(m.view()) {
		t.Errorf("selection %d is past the end of %d services", m.selected, len(m.view()))
	}
}

// A sandbox's ceiling is the sum of its services'.
func TestASandboxsCeilingIsTheSumOfItsServices(t *testing.T) {
	m := model{
		rows:    twoSandboxes(),
		grouped: true,
		limits: map[string]provider.Limits{
			"r1": {NanoCPUs: 1e9, MemBytes: 512 << 20},
			"r2": {NanoCPUs: 500_000_000, MemBytes: 128 << 20},
		},
	}

	got := m.limitsFor(m.view()[0])

	if got.NanoCPUs != 1_500_000_000 || got.MemBytes != 640<<20 {
		t.Errorf("work is allowed %+v, want 1.5 cores and 640 MB", got)
	}
}

// ... but only where every service has one. Three capped and a fourth without is a sandbox
// nothing bounds - the fourth can take the machine - so the total of the three would be a
// figure that looks like a ceiling and holds nothing back.
func TestOneUncappedServiceMeansTheSandboxIsUncapped(t *testing.T) {
	m := model{
		rows:    twoSandboxes(),
		grouped: true,
		limits:  map[string]provider.Limits{"r1": {NanoCPUs: 1e9, MemBytes: 512 << 20}},
	}

	if got := m.limitsFor(m.view()[0]); got.Capped() {
		t.Errorf("work reports a ceiling of %+v while redis has none", got)
	}
}

// The sandbox's graph is its services added together at each moment, lined up from the newest
// end: one created later has a shorter history, and lining them up from the start would add
// this second's reading to one from three minutes ago.
func TestTheSandboxGraphAddsItsServicesAtEachMoment(t *testing.T) {
	m := model{
		rows:    twoSandboxes(),
		grouped: true,
		series: map[string][]metricSample{
			"r1": {{cores: 1, mem: 100, known: true}, {cores: 2, mem: 200, known: true},
				{cores: 3, mem: 300, known: true}},
			"r2": {{cores: 10, mem: 1000, known: true}}, // younger: one sample only
		},
	}

	got := m.seriesFor(m.view()[0])

	if len(got) != 3 {
		t.Fatalf("the sandbox series is %d samples, want the longest history's 3", len(got))
	}

	// The newest sample has both; the older two have only the service that existed then.
	if got[2].cores != 13 || got[2].mem != 1300 {
		t.Errorf("newest sample = %+v, want both services added", got[2])
	}

	if got[0].cores != 1 || got[0].mem != 100 {
		t.Errorf("oldest sample = %+v, want only the service that existed then", got[0])
	}
}

// The sandbox total says whether it is costing anything and cannot say which service it went
// to, which is the next question and the reason somebody opened the sandbox at all.
func TestEachServiceShowsItsOwnShareAndShape(t *testing.T) {
	rows := twoSandboxes() // work: mysql 300 MB, redis 4 MB

	series := map[string][]metricSample{}
	for _, r := range rows {
		for range 60 {
			series[r.Ref] = append(series[r.Ref], metricSample{mem: r.MemBytes, known: true})
		}
	}

	frame := plain(render(model{
		version: "v0", rows: rows, grouped: true, metered: true, series: series,
	}, 26, 130))

	// 300 of 304 MB is arithmetic a reader should not have to do to find the expensive one.
	if !strings.Contains(frame, "99%") {
		t.Errorf("mysql's share of the sandbox is not shown:\n%s", frame)
	}

	if !strings.Contains(frame, "1%") {
		t.Errorf("redis's share of the sandbox is not shown:\n%s", frame)
	}

	// And a shape per service, because a flat line beside a climbing one is the thing a column
	// of numbers cannot show.
	if !strings.Contains(frame, "⠒") && !strings.Contains(frame, "⠉") {
		t.Errorf("no per-service trace was drawn:\n%s", frame)
	}

	// Both figures, because the table's headers promise CPU and MEMORY and the sandbox's own
	// block gives both. Memory alone says which service is resident; cpu says which is working.
	if !strings.Contains(frame, "5.0%") {
		t.Errorf("no per-service cpu was shown, only memory:\n%s", frame)
	}
}

// A sleeping service has nothing to measure, and a zero would be a measurement.
func TestASleepingServiceShowsNoShare(t *testing.T) {
	rows := twoSandboxes()
	rows[1].Awake, rows[1].MemKnown = false, false

	frame := plain(render(model{version: "v0", rows: rows, grouped: true, metered: true}, 26, 130))

	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "redis") && strings.Contains(line, "%") {
			t.Errorf("a sleeping service was given a share of the memory: %q", line)
		}
	}
}

// Every member line agrees with the block's own heading, whatever is in it.
//
// This used to assert that the readings landed under the *table's* CPU and MEMORY columns
// instead. Honouring that meant cutting an address to fit - a service publishing two ports
// carries thirty-one columns of them - and nothing on this screen is worth hiding a port for.
// So the block has a heading of its own, its columns are measured from what is in it, and the
// thing to assert is that the heading and the rows are one grid.
func TestEveryMemberLineAgreesWithItsHeading(t *testing.T) {
	rows := []row{
		{Sandbox: "work", Service: "clickhouse", Awake: true, Ref: "r1",
			Address: "127.0.0.1:20002 127.0.0.1:20003",
			CPU:     20.7, CPUKnown: true, MemBytes: 364 << 20, MemKnown: true},
		{Sandbox: "work", Service: "mysql", Awake: true, Ref: "r2", Address: "127.0.0.1:20000",
			CPU: 5.3, CPUKnown: true, MemBytes: 100 << 20, MemKnown: true},
		{Sandbox: "work", Service: "redis", Awake: false, Ref: "r3", Address: "127.0.0.1:20001"},
	}

	frame := plain(render(model{version: "v0", rows: rows, grouped: true, metered: true}, 30, 150))

	end := func(line, token string) int {
		i := strings.Index(line, token)
		if i < 0 {
			return -1
		}

		return utf8.RuneCountInString(line[:i]) + utf8.RuneCountInString(token)
	}

	find := func(prefix string) string {
		for _, l := range strings.Split(frame, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), prefix) {
				return l
			}
		}

		return ""
	}

	head, wide, narrow := find("SERVICE"), find("clickhouse"), find("mysql")

	if head == "" || wide == "" || narrow == "" {
		t.Fatalf("could not find the heading and two rows:\n%s", frame)
	}

	for _, c := range []struct{ what, h, a, b string }{
		{"cpu", "CPU", "20.7%", "5.3%"},
		{"memory", "MEMORY", "364m", "100m"},
		{"share", "SHARE", "78%", "22%"},
	} {
		x, y, z := end(head, c.h), end(wide, c.a), end(narrow, c.b)

		if x < 0 || y < 0 || z < 0 {
			t.Errorf("%s: missing a column (heading %d, wide %d, narrow %d)\n%s",
				c.what, x, y, z, frame)

			continue
		}

		if x != y || y != z {
			t.Errorf("%s ends at %d in the heading, %d on the two-port row and %d on the other\n%s",
				c.what, x, y, z, frame)
		}
	}

	// And both ports are still there. The alignment was never worth a port for.
	if !strings.Contains(frame, "127.0.0.1:20002 127.0.0.1:20003") {
		t.Errorf("an address was cut to make the columns line up:\n%s", frame)
	}
}

// Where the sandbox has a short name the table's columns sit left of where the address ends,
// and there is nothing to align to. What must not happen is the reading landing against the
// address: "127.0.0.1:1" and "5.0%" were once printed as "127.0.0.1:15.0%".
func TestAReadingNeverRunsIntoTheAddress(t *testing.T) {
	frame := plain(render(model{
		version: "v0", rows: twoSandboxes(), grouped: true, metered: true,
	}, 26, 132))

	for _, l := range strings.Split(frame, "\n") {
		if !strings.Contains(l, "127.0.0.1:") {
			continue
		}

		if regexpDigitPct.MatchString(l) {
			t.Errorf("a reading ran into the address: %q", strings.TrimRight(l, " "))
		}
	}
}

// An address followed immediately by a percentage, with no space between them.
var regexpDigitPct = regexp.MustCompile(`127\.0\.0\.1:\d+\.?\d*%`)

// A service publishing two ports carries an address twice as wide as its neighbours'. Left to
// run, it pushed its own readings out of the column the other lines shared - so the one line
// somebody was comparing against the others was the one that did not line up.
func TestOneWideAddressDoesNotBreakTheColumn(t *testing.T) {
	rows := []row{
		{Sandbox: "work", Service: "clickhouse", Awake: true, Ref: "r1",
			Address: "127.0.0.1:20002 127.0.0.1:20003",
			CPU:     20.7, CPUKnown: true, MemBytes: 364 << 20, MemKnown: true},
		{Sandbox: "work", Service: "mysql", Awake: true, Ref: "r2", Address: "127.0.0.1:20000",
			CPU: 5.3, CPUKnown: true, MemBytes: 362 << 20, MemKnown: true},
		{Sandbox: "work", Service: "redis", Awake: true, Ref: "r3", Address: "127.0.0.1:20001",
			CPU: 2.5, CPUKnown: true, MemBytes: 4 << 20, MemKnown: true},
	}

	frame := plain(render(model{
		version: "v0", rows: rows, grouped: true, metered: true,
	}, 26, 132))

	end := func(name, token string) int {
		for _, l := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), name) {
				continue
			}

			i := strings.Index(l, token)
			if i < 0 {
				return -1
			}

			return utf8.RuneCountInString(l[:i]) + utf8.RuneCountInString(token)
		}

		return -1
	}

	for _, c := range []struct{ what, a, b, cc string }{
		{"cpu", "20.7%", "5.3%", "2.5%"},
		{"memory", "364m", "362m", "4m"},
	} {
		x, y, z := end("clickhouse", c.a), end("mysql", c.b), end("redis", c.cc)

		if x < 0 || y < 0 || z < 0 {
			t.Errorf("%s: a member line is missing its reading (%d, %d, %d)\n%s",
				c.what, x, y, z, frame)

			continue
		}

		if x != y || y != z {
			t.Errorf("%s ends at %d for the two-port service and %d/%d for the others - the "+
				"wide address took the column with it\n%s", c.what, x, y, z, frame)
		}
	}

	// And it still says both ports, rather than quietly dropping one to make the sums work.
	if !strings.Contains(frame, "127.0.0.1:20002 127.0.0.1:20003") {
		t.Errorf("the second address was dropped:\n%s", frame)
	}
}
