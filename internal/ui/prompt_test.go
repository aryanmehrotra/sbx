package ui

// Typing a value into a dashboard driven by single keys.
//
// The risk here is not that the prompt looks wrong. It is that a key meant for the buffer is
// read as a command - "512m" ends in the letter that sleeps a service - or that the answer
// lands on whatever happens to be selected when it is submitted rather than on the service it
// was opened for.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/tui"
)

// limiterProvider is a fake that records the ceilings it was asked to set.
type limiterProvider struct {
	fakeProvider

	lmu  sync.Mutex
	got  provider.Limits
	ref  string
	sets int
	have provider.Limits

	// refuse, when set, is the error this backend answers SetLimits with - standing in for a
	// docker daemon that will not remove a ceiling.
	refuse string
}

func (l *limiterProvider) Limits(context.Context, string) (provider.Limits, error) {
	l.lmu.Lock()
	defer l.lmu.Unlock()

	return l.have, nil
}

func (l *limiterProvider) SetLimits(_ context.Context, ref string, want provider.Limits) error {
	l.lmu.Lock()
	defer l.lmu.Unlock()

	l.got, l.ref, l.sets = want, ref, l.sets+1

	if l.refuse != "" {
		return errors.New(l.refuse)
	}

	l.have = want

	return nil
}

func (l *limiterProvider) taken() (provider.Limits, string, int) {
	l.lmu.Lock()
	defer l.lmu.Unlock()

	return l.got, l.ref, l.sets
}

func rkey(r rune) tui.Key { return tui.Key{Rune: r, Code: tui.KeyRune} }

// typeInto drives the dashboard the way a person would: one key at a time.
func typeInto(t *testing.T, d *dash, s string) {
	t.Helper()

	for _, r := range s {
		switch r {
		case '\r':
			d.handle(context.Background(), tui.Key{Code: tui.KeyEnter})
		case '\b':
			d.handle(context.Background(), tui.Key{Code: tui.KeyBackspace})
		default:
			d.handle(context.Background(), rkey(r))
		}
	}
}

// waitFor gives the goroutine that talks to the provider a moment to finish. The dashboard
// never blocks its keyboard on a round trip, so the effect of a keypress lands after it.
func waitFor(t *testing.T, done func() bool) {
	t.Helper()

	for range 200 {
		if done() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("the provider was never asked, after a second of waiting")
}

func dashWithLimiter() (*dash, *limiterProvider) {
	p := &limiterProvider{}
	d := newDash(p)
	d.model.rows = []row{
		{Sandbox: "one", Service: "db", Ref: "sbx-one-db", Awake: true},
		{Sandbox: "two", Service: "cache", Ref: "sbx-two-cache", Awake: true},
	}

	return d, p
}

func TestLOpensAPromptForTheSelectedService(t *testing.T) {
	d, _ := dashWithLimiter()
	d.model.selected = 1

	d.handle(context.Background(), rkey('L'))

	if !d.model.input.active {
		t.Fatal("L did not open a prompt")
	}

	if d.model.input.ref != "sbx-two-cache" {
		t.Errorf("the prompt is for %q, want the selected service sbx-two-cache",
			d.model.input.ref)
	}
}

// Every key belongs to the buffer while it is open. "512m" ends in the letter that sleeps a
// service, and "l" would open the log pane - a prompt that let those through would act on the
// fleet while somebody was typing a number.
func TestAPromptSwallowsCommandKeys(t *testing.T) {
	d, p := dashWithLimiter()

	d.handle(context.Background(), rkey('L'))
	d.handle(context.Background(), rkey('c')) // write our own rather than pick a size
	typeInto(t, d, "0.5,512m")

	if d.model.input.buffer != "0.5,512m" {
		t.Fatalf("buffer = %q, want 0.5,512m", d.model.input.buffer)
	}

	if stopped, _ := p.took(); len(stopped) != 0 {
		t.Errorf("typing put %v to sleep - a key meant for the buffer was read as a command",
			stopped)
	}

	if d.model.pane == paneLogs {
		t.Error("typing opened the log pane; the l in a value was read as a command")
	}
}

func TestBackspaceAndEscape(t *testing.T) {
	d, _ := dashWithLimiter()

	d.handle(context.Background(), rkey('L'))
	d.handle(context.Background(), rkey('c')) // write our own rather than pick a size
	typeInto(t, d, "2,4g\b\b")

	if d.model.input.buffer != "2," {
		t.Errorf("after two backspaces buffer = %q, want %q", d.model.input.buffer, "2,")
	}

	d.handle(context.Background(), tui.Key{Code: tui.KeyEscape})

	if d.model.input.active {
		t.Error("escape left the prompt open")
	}
}

func TestSubmittingSetsTheLimit(t *testing.T) {
	d, p := dashWithLimiter()

	d.handle(context.Background(), rkey('L'))
	d.handle(context.Background(), rkey('c')) // write our own rather than pick a size
	typeInto(t, d, "0.5,256m\r")

	waitFor(t, func() bool { _, _, n := p.taken(); return n == 1 })

	got, ref, _ := p.taken()

	if ref != "sbx-one-db" {
		t.Errorf("limits went to %q, want sbx-one-db", ref)
	}

	if got.NanoCPUs != 500_000_000 {
		t.Errorf("NanoCPUs = %d, want 500000000 (half a core)", got.NanoCPUs)
	}

	if got.MemBytes != 256<<20 {
		t.Errorf("MemBytes = %d, want %d", got.MemBytes, 256<<20)
	}

	if d.model.input.active {
		t.Error("the prompt stayed open after submitting")
	}
}

// The selection moves under a prompt every second as the fleet refreshes. The answer has to
// land on the service the prompt was opened for.
func TestTheAnswerFollowsThePromptNotTheSelection(t *testing.T) {
	d, p := dashWithLimiter()

	d.handle(context.Background(), rkey('L')) // opened on row 0, sbx-one-db
	d.handle(context.Background(), rkey('c'))
	typeInto(t, d, "1,512m")

	d.model.selected = 1 // the fleet shifted, or an arrow slipped through elsewhere

	typeInto(t, d, "\r")

	waitFor(t, func() bool { _, _, n := p.taken(); return n == 1 })

	if _, ref, _ := p.taken(); ref != "sbx-one-db" {
		t.Errorf("limits landed on %q, want the service the prompt was opened for, "+
			"sbx-one-db", ref)
	}
}

// Naming one half must not silently clear the other.
func TestAnUnmentionedHalfIsLeftAlone(t *testing.T) {
	d, p := dashWithLimiter()

	p.have = provider.Limits{NanoCPUs: 2e9, MemBytes: 1 << 30}
	d.model.limits = map[string]provider.Limits{"sbx-one-db": p.have}

	d.handle(context.Background(), rkey('L'))
	d.handle(context.Background(), rkey('c')) // write our own rather than pick a size
	typeInto(t, d, "0.5\r")                   // cpu only

	waitFor(t, func() bool { _, _, n := p.taken(); return n == 1 })

	got, _, _ := p.taken()

	if got.NanoCPUs != 500_000_000 {
		t.Errorf("NanoCPUs = %d, want the half a core that was typed", got.NanoCPUs)
	}

	if got.MemBytes != 1<<30 {
		t.Errorf("MemBytes = %d, want the 1 GB that was already set and not mentioned",
			got.MemBytes)
	}
}

// Whether a ceiling can be removed at all is the backend's rule, not the dashboard's: docker
// cannot, a cluster can. So "none" is passed down rather than refused here, and whatever the
// backend says comes back to the footer unedited.
func TestClearingIsTheProvidersDecision(t *testing.T) {
	d, p := dashWithLimiter()

	p.have = provider.Limits{NanoCPUs: 2e9, MemBytes: 1 << 30}
	d.model.limits = map[string]provider.Limits{"sbx-one-db": p.have}
	p.refuse = "docker cannot remove a cpu limit from a container that exists - " +
		"recreate the sandbox to clear it"

	d.handle(context.Background(), rkey('L'))
	d.handle(context.Background(), rkey('c'))
	typeInto(t, d, "none\r")

	waitFor(t, func() bool { return d.model.message != "" })

	if _, _, n := p.taken(); n != 1 {
		t.Errorf("the backend was asked %d times; it is the one that decides whether a "+
			"ceiling can be removed", n)
	}

	if !strings.Contains(d.model.message, "recreate") {
		t.Errorf("message = %q, want the backend's own refusal", d.model.message)
	}
}

// And where the backend allows it, clearing goes through.
func TestClearingGoesThroughWhereTheBackendAllowsIt(t *testing.T) {
	d, p := dashWithLimiter()

	p.have = provider.Limits{NanoCPUs: 2e9, MemBytes: 1 << 30}
	d.model.limits = map[string]provider.Limits{"sbx-one-db": p.have}

	d.handle(context.Background(), rkey('L'))
	d.handle(context.Background(), rkey('c'))
	typeInto(t, d, "none\r")

	waitFor(t, func() bool { _, _, n := p.taken(); return n == 1 })

	if got, _, _ := p.taken(); got.Capped() {
		t.Errorf("clearing sent %+v, want both ceilings zeroed", got)
	}
}

// An empty line is a mistake rather than a decision, and is the one case the dashboard does
// turn away itself.
func TestAnEmptyLineIsNotAClear(t *testing.T) {
	d, p := dashWithLimiter()

	d.handle(context.Background(), rkey('L'))
	d.handle(context.Background(), rkey('c'))
	typeInto(t, d, "\r")

	waitFor(t, func() bool { return d.model.message != "" })

	if _, _, n := p.taken(); n != 0 {
		t.Errorf("an empty line reached the backend %d times", n)
	}
}

// Asking to clear something that is already uncapped is not a request to clear anything, and
// must not be refused on a technicality.
func TestNoneOnAnAlreadyUncappedHalfIsNotRefused(t *testing.T) {
	d, p := dashWithLimiter()

	p.have = provider.Limits{MemBytes: 1 << 30} // cpu already uncapped
	d.model.limits = map[string]provider.Limits{"sbx-one-db": p.have}

	d.handle(context.Background(), rkey('L'))
	d.handle(context.Background(), rkey('c')) // write our own rather than pick a size
	typeInto(t, d, "none,2g\r")

	waitFor(t, func() bool { _, _, n := p.taken(); return n == 1 })

	if got, _, _ := p.taken(); got.MemBytes != 2<<30 {
		t.Errorf("MemBytes = %d, want the 2 GB that was asked for", got.MemBytes)
	}
}

// The common case is one of the offered sizes, and it must be one keypress rather than a
// syntax to remember.
func TestPickingAnOfferedSize(t *testing.T) {
	for _, p := range limitPresets {
		d, prov := dashWithLimiter()

		d.handle(context.Background(), rkey('L'))

		if d.model.input.typing {
			t.Fatal("L went straight to a text field; the sizes were never offered")
		}

		d.handle(context.Background(), rkey(p.key))

		waitFor(t, func() bool { _, _, n := prov.taken(); return n == 1 })

		got, ref, _ := prov.taken()

		if got != p.limits {
			t.Errorf("%q set %+v, want %+v", p.name, got, p.limits)
		}

		if ref != "sbx-one-db" {
			t.Errorf("%q went to %q, want the selected service", p.name, ref)
		}

		if d.model.input.active {
			t.Errorf("%q left the chooser open", p.name)
		}
	}
}

// c is the way out to a value nobody thought to offer.
func TestCustomOpensTheTextField(t *testing.T) {
	d, _ := dashWithLimiter()

	d.handle(context.Background(), rkey('L'))
	d.handle(context.Background(), rkey('c'))

	if !d.model.input.typing {
		t.Fatal("c did not open the text field")
	}

	typeInto(t, d, "0.75,700m")

	if d.model.input.buffer != "0.75,700m" {
		t.Errorf("buffer = %q, want 0.75,700m", d.model.input.buffer)
	}
}

// While the sizes are offered, a key that is not one of them must do nothing at all - and in
// particular must not fall through to the fleet commands underneath.
func TestAnUnofferedKeyDoesNothingWhileChoosing(t *testing.T) {
	d, p := dashWithLimiter()

	d.handle(context.Background(), rkey('L'))

	for _, r := range []rune{'9', 'd', 's', 'q', 'z'} {
		d.handle(context.Background(), rkey(r))
	}

	if !d.model.input.active {
		t.Error("a key that was not offered closed the chooser")
	}

	if stopped, removed := p.took(); len(stopped) != 0 || len(removed) != 0 {
		t.Errorf("keys leaked to the fleet while choosing: stopped=%v removed=%v",
			stopped, removed)
	}
}

// Escape gets out of both steps.
func TestEscapeLeavesTheChooser(t *testing.T) {
	d, _ := dashWithLimiter()

	d.handle(context.Background(), rkey('L'))
	d.handle(context.Background(), tui.Key{Code: tui.KeyEscape})

	if d.model.input.active {
		t.Error("escape did not close the chooser")
	}
}

// Every offered size has to fit the footer it is drawn in, at a narrow terminal too.
func TestTheChooserFitsItsFooter(t *testing.T) {
	for _, cols := range []int{40, 64, 80, 118, 200} {
		got := footer(model{input: prompt{active: true, label: "limit zn-dev/clickhouse"}}, 5, cols)

		if n := visibleLen(got); n > cols {
			t.Errorf("at %d columns the chooser is %d wide: %q", cols, n, plainText(got))
		}
	}
}

func TestParsingWhatSomebodyWouldType(t *testing.T) {
	for _, c := range []struct {
		cpu, mem string
		cores    int64
		bytes    uint64
		wantErr  bool
	}{
		{"0.5", "256m", 500_000_000, 256 << 20, false},
		{"2", "4g", 2_000_000_000, 4 << 30, false},
		{"1", "512mb", 1_000_000_000, 512 << 20, false},
		{"", "", 0, 0, false},
		{"none", "none", 0, 0, false},
		{"half", "256m", 0, 0, true},
		{"0.5", "tiny", 0, 0, true},
		{"-1", "256m", 0, 0, true},
		{"0.5", "1m", 0, 0, true}, // under docker's 6m floor
	} {
		got, err := provider.ParseLimits(c.cpu, c.mem)

		if c.wantErr {
			if err == nil {
				t.Errorf("ParseLimits(%q, %q) accepted a value it cannot mean", c.cpu, c.mem)
			}

			continue
		}

		if err != nil {
			t.Errorf("ParseLimits(%q, %q) = %v", c.cpu, c.mem, err)

			continue
		}

		if got.NanoCPUs != c.cores || got.MemBytes != c.bytes {
			t.Errorf("ParseLimits(%q, %q) = %d nanocpus, %d bytes; want %d, %d",
				c.cpu, c.mem, got.NanoCPUs, got.MemBytes, c.cores, c.bytes)
		}
	}
}
