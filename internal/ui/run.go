package ui

// The live half: poll, draw, react to keys.
//
// Everything that can be decided without a terminal lives in model.go and render.go. This
// file is the part that has to talk to a docker daemon and a tty, and it is kept small for
// that reason.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/tui"
	"github.com/aryanmehrotra/sbx/internal/update"
)

// Refresh is how often the fleet is re-listed. A second is fast enough that waking something
// from another terminal shows up while you are still looking, and slow enough that twenty
// sandboxes do not keep a laptop's docker daemon busy.
const Refresh = time.Second

// Options carries what Run needs from the caller.
type Options struct {
	Provider provider.Provider
	Version  string
	Repo     string
}

// Run draws the dashboard until the user quits.
//
// Where there is no terminal - a pipe, a CI job, Windows without WSL2 - it prints the table
// once and returns, because `sbx ui | grep` should do something sensible rather than fail.
func Run(ctx context.Context, opt Options, out *os.File) error {
	if !tui.IsTerminal(out) {
		return printOnce(ctx, opt, out)
	}

	screen, err := tui.Open(out)
	if err != nil {
		if errors.Is(err, tui.ErrUnsupported) {
			return printOnce(ctx, opt, out)
		}

		return err
	}
	defer screen.Close()

	// Asks GitHub at most once a day, in the background, and only ever affects the *next*
	// run. Nothing here waits for it.
	update.Refresh(opt.Repo)

	d := &dash{
		opt:   opt,
		model: model{version: opt.Version, update: update.Available(opt.Version)},
		prev:  map[string]provider.Usage{},
	}

	d.refresh(ctx)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The poll runs on its own clock so that a slow docker daemon delays the data rather than
	// the keyboard: a dashboard that stops responding to `q` while it waits on a listing is
	// one people kill with ^C.
	go d.poll(ctx)

	reader := tui.NewReader(os.Stdin)

	for {
		rows, cols := screen.Size()

		d.mu.Lock()
		frame := render(d.model, rows, cols)
		d.mu.Unlock()

		screen.Draw(frame)

		select {
		case <-ctx.Done():
			return nil
		case <-screen.Resized:
			continue
		default:
		}

		key, ok := reader.Read()
		if !ok {
			continue // the read timed out, which is how a redraw happens with nobody typing
		}

		// ^Z belongs to the shell, not to the dashboard. It is handled here rather than in
		// handle() because suspending is the screen's business and handle() has no screen.
		if key.Code == tui.KeyCtrlZ {
			screen.Suspend()

			continue
		}

		if quit := d.handle(ctx, key); quit {
			return nil
		}
	}
}

// dash is the running dashboard.
type dash struct {
	opt Options

	mu    sync.Mutex
	model model

	// prev is the previous CPU sample per ref, which is what makes a rate possible.
	prev map[string]provider.Usage

	// paneHeight is the height the renderer last drew the pane at, so the scroll keys can
	// bound themselves without the model having to know the terminal's size.
	paneHeight int
}

// eventBacklog is how many events the refresher keeps in memory. It is a ceiling on
// what the log pane can ever show, so it must exceed the tallest plausible terminal
// rather than the shortest — the renderer trims to fit, this only has to not run out.
const eventBacklog = 500

func (d *dash) poll(ctx context.Context) {
	t := time.NewTicker(Refresh)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.refresh(ctx)
			d.followLogs(ctx)
			d.followLimits(ctx)
		}
	}
}

// refresh re-lists the fleet and re-samples usage.
//
// No lock is held across a network call. Listing and sampling are round trips to a docker
// daemon that can take hundreds of milliseconds against a busy one, and holding the model's
// mutex across them stalls the redraw and the keyboard with it - a dashboard that stops
// responding to `q` while it waits on docker is one people kill with ^C.
func (d *dash) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	units, err := d.opt.Provider.List(ctx, "")
	if err != nil {
		d.mu.Lock()
		d.model.err = err
		d.mu.Unlock()

		return
	}

	rows := rowsFrom(units)

	// Only what is running is sampled. An asleep sandbox has no process to ask, and asking
	// anyway would be a round trip per sleeping sandbox per second to learn nothing.
	var running []string

	for _, r := range rows {
		if r.Awake {
			running = append(running, r.Ref)
		}
	}

	var now map[string]provider.Usage

	if m, ok := d.opt.Provider.(provider.Meter); ok && len(running) > 0 {
		now, _ = m.Stats(ctx, running)
	}

	// Read enough to fill any terminal and let the renderer decide how many fit. The
	// screen size is only known at render time, and a limit of three here made the
	// layout question moot: there was never a fourth line to show.
	events, _ := history.Read(history.Filter{Kind: "event", Limit: eventBacklog})

	// Everything below is arithmetic and assignment, so the lock is held for microseconds.
	d.mu.Lock()
	defer d.mu.Unlock()

	d.model.err = nil

	for i := range rows {
		cur, ok := now[rows[i].Ref]
		if !ok {
			continue
		}

		rows[i].MemBytes, rows[i].MemKnown = cur.MemBytes, true
		rows[i].OnlineCPUs = cur.OnlineCPUs

		if prev, ok := d.prev[rows[i].Ref]; ok {
			rows[i].CPU, rows[i].CPUKnown = cpuPercent(prev, cur)
		}
	}

	if now != nil {
		d.prev = now
	}

	d.model.rows = rows

	if d.model.selected >= len(rows) {
		d.model.selected = max(0, len(rows)-1)
	}

	if events != nil {
		d.model.events = events
		d.model.stats = summarise(events)
	}

	d.model.provider = d.opt.Provider.Name()
}

// handle applies a keypress. It returns true when the user is finished.
func (d *dash) handle(ctx context.Context, k tui.Key) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	// A prompt takes every key, because while somebody is typing "512m" the m is a character
	// and not a command. Nothing below this line runs until they finish or give up.
	if d.model.input.active {
		d.typing(ctx, k)

		return false
	}

	// A pending confirmation swallows everything except its own answer, so that a stray
	// keypress cannot remove a sandbox.
	if d.model.confirm != "" {
		if k.Rune == 'y' {
			d.model.confirm = ""

			go d.remove(context.WithoutCancel(ctx), d.current())
		} else {
			d.model.confirm = ""
			d.model.message = "left alone"
		}

		return false
	}

	// Escape always returns the arrows to the table. Whatever else is going on, there is one
	// key that gets you back to the thing the dashboard is about.
	if k.Code == tui.KeyEscape {
		d.model.focus = focusTable

		return false
	}

	// Tab moves the arrows to the pane and back, and refuses to move them somewhere they
	// would do nothing.
	//
	// Focus used to follow `l`, which is how somebody pressed l to look at a log, pressed down
	// to move to the next service, and watched nothing happen: the arrows were scrolling a
	// pane whose contents already fitted. The table is what this screen is about, so it keeps
	// the arrows unless they are explicitly asked for elsewhere.
	if k.Code == tui.KeyTab {
		if d.model.focus == focusPane {
			d.model.focus = focusTable

			return false
		}

		if maxOffset(d.model, max(1, d.paneHeight)) == 0 {
			d.model.message = "nothing to scroll: it is all on screen"
			d.model.messageAt = time.Now()

			return false
		}

		d.model.focus = focusPane

		return false
	}

	if d.model.focus == focusPane && d.scrollPane(k) {
		return false
	}

	switch {
	case k.Code == tui.KeyCtrlC, k.Rune == 'q':
		return true

	case k.Code == tui.KeyUp, k.Rune == 'k':
		d.model.selected = max(0, d.model.selected-1)

		d.followSelection(ctx)

	case k.Code == tui.KeyDown, k.Rune == 'j':
		// max(0, ...) because an empty fleet makes len-1 negative, and a selection of -1 is a
		// row that does not exist waiting for the first piece of code that indexes without
		// checking. Nothing does today; that is luck, not design.
		d.model.selected = max(0, min(len(d.model.rows)-1, d.model.selected+1))

		d.followSelection(ctx)

	case k.Code == tui.KeyEnter:
		go d.wake(context.WithoutCancel(ctx), d.current())

	case k.Rune == 's':
		go d.sleep(context.WithoutCancel(ctx), d.current())

	case k.Rune == 'l':
		// A toggle, not a screen. The pane's contents change and the layout does not move.
		if d.model.pane == paneLogs {
			d.model.pane = paneEvents
		} else {
			d.model.pane = paneLogs

			go d.loadLogs(context.WithoutCancel(ctx), d.current())
		}

		d.model.offset = 0

	case k.Rune == 'L':
		if r, ok := d.model.currentRow(); ok {
			d.model.input = prompt{
				active: true,
				label:  fmt.Sprintf("limit %s/%s — cpu,memory", r.Sandbox, r.Service),
				ref:    r.Ref,
				name:   r.Sandbox + "/" + r.Service,
			}
		}

	case k.Rune == 'd':
		if r, ok := d.model.currentRow(); ok {
			d.model.confirm = fmt.Sprintf("remove %s and its data?", r.Sandbox)
		}

	case k.Rune == 'r':
		go d.refresh(context.WithoutCancel(ctx))
	}

	return false
}

// scrollPane moves the pane's window. It reports whether the key was one of its own, so that
// anything else falls through to the table's bindings rather than being swallowed by a focus
// the reader may have forgotten about.
//
// The bounds need the pane's height, which only the renderer knows exactly. paneHeight is the
// last height it drew at, which is right except for the frame after a resize.
func (d *dash) scrollPane(k tui.Key) bool {
	h := max(1, d.paneHeight)

	limit := maxOffset(d.model, h)

	switch {
	case k.Code == tui.KeyUp, k.Rune == 'k':
		d.model.offset = min(limit, d.model.offset+1)
	case k.Code == tui.KeyDown, k.Rune == 'j':
		d.model.offset = max(0, d.model.offset-1)
	case k.Code == tui.KeyPageUp:
		d.model.offset = min(limit, d.model.offset+h)
	case k.Code == tui.KeyPageDown:
		d.model.offset = max(0, d.model.offset-h)
	case k.Code == tui.KeyHome, k.Rune == 'g':
		d.model.offset = limit
	case k.Code == tui.KeyEnd, k.Rune == 'G':
		d.model.offset = 0 // following again
	default:
		return false
	}

	return true
}

func (d *dash) current() row {
	r, _ := d.model.currentRow()

	return r
}

func (d *dash) say(format string, a ...any) {
	d.mu.Lock()
	d.model.message = fmt.Sprintf(format, a...)
	d.model.messageAt = time.Now()
	d.mu.Unlock()
}

// wake connects to the service, because connecting is the only way this product wakes
// anything. Calling the provider's Start directly would demonstrate a path no user has.
//
// The dial returning is not the wake. The daemon accepts immediately and only then starts the
// container, so a dial-and-close comes back in about a millisecond and reports nothing: the
// first version printed "woke in 1ms" for a postgres that had not started yet, which reads as
// a dashboard that is lying or stuck. What is timed instead is the round trip to a service
// that is actually running again.
func (d *dash) wake(ctx context.Context, r row) {
	if r.Address == "" {
		return
	}

	if r.Awake {
		d.say("%s/%s is already awake", r.Sandbox, r.Service)

		return
	}

	addr := strings.Fields(r.Address)[0]

	d.say("waking %s/%s ...", r.Sandbox, r.Service)

	start := time.Now()

	conn, err := net.DialTimeout("tcp", addr, 90*time.Second)
	if err != nil {
		d.say("could not wake %s/%s: %v", r.Sandbox, r.Service, err)

		return
	}

	// Held open until the workload is up. Closing at once is what the daemon sees as a caller
	// that changed its mind, and it is also what made the measurement meaningless.
	defer conn.Close()

	if err := d.waitAwake(ctx, r.Ref, 90*time.Second); err != nil {
		d.say("%s/%s did not come up: %v", r.Sandbox, r.Service, err)

		return
	}

	d.say("%s/%s woke in %dms", r.Sandbox, r.Service, time.Since(start).Milliseconds())

	d.refresh(ctx)
}

// waitAwake polls until the backend reports the unit running.
//
// Polling rather than trusting the connection, because "the daemon accepted" and "postgres is
// serving" are different facts and only the second one is worth putting on screen.
func (d *dash) waitAwake(ctx context.Context, ref string, limit time.Duration) error {
	deadline := time.Now().Add(limit)

	for {
		units, err := d.opt.Provider.List(ctx, "")
		if err == nil {
			for _, u := range units {
				if u.Ref == ref && u.Running {
					return nil
				}
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("still not running after %s", limit)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (d *dash) sleep(ctx context.Context, r row) {
	if !r.Awake {
		d.say("%s/%s is already asleep", r.Sandbox, r.Service)

		return
	}

	if err := d.opt.Provider.Stop(ctx, r.Ref); err != nil {
		d.say("could not sleep %s/%s: %v", r.Sandbox, r.Service, err)

		return
	}

	d.say("%s/%s asleep", r.Sandbox, r.Service)

	d.refresh(ctx)
}

func (d *dash) remove(ctx context.Context, r row) {
	if r.Sandbox == "" {
		return
	}

	if err := d.opt.Provider.Remove(ctx, r.Sandbox); err != nil {
		d.say("could not remove %s: %v", r.Sandbox, err)

		return
	}

	d.say("removed %s", r.Sandbox)

	d.refresh(ctx)
}

// loadLogs fills the bottom pane with the selected service's output.
//
// Fetched when the pane is opened and when the selection moves, not on every tick: a log read
// per second per sandbox is a lot of round trips to say nothing new, and the pane is a glance
// rather than a follow.
func (d *dash) loadLogs(ctx context.Context, r row) {
	if r.Ref == "" {
		return
	}

	var b strings.Builder

	if err := d.opt.Provider.Logs(ctx, r.Ref, 200, false, &b); err != nil {
		d.say("could not read logs: %v", err)

		return
	}

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")

	d.mu.Lock()
	defer d.mu.Unlock()

	// The selection may have moved while this was in flight, and showing one service's logs
	// under another's name is worse than showing none.
	if cur, ok := d.model.currentRow(); !ok || cur.Ref != r.Ref {
		return
	}

	d.model.logs = lines
}

// followLogs re-reads the selected service's output while the pane is open and following.
//
// Only while following. Somebody who has scrolled back is reading something, and refetching
// under them would either move the text or silently change what "40 lines back" means; the
// pane says "following" or gives a position, and this respects which of those it said.
//
// Only while the pane is open, because a log read per service per second is a lot of round
// trips to a docker daemon to answer a question nobody is asking.
func (d *dash) followLogs(ctx context.Context) {
	d.mu.Lock()

	if d.model.pane != paneLogs || d.model.offset != 0 {
		d.mu.Unlock()

		return
	}

	r, ok := d.model.currentRow()

	d.mu.Unlock()

	if !ok {
		return
	}

	d.loadLogs(ctx, r)
}

// followSelection keeps the log pane pointed at the highlighted row. Called with the lock
// held, so the fetch itself is a goroutine.
func (d *dash) followSelection(ctx context.Context) {
	r, ok := d.model.currentRow()

	if d.model.pane == paneLogs {
		d.model.logs = nil

		if ok {
			go d.loadLogs(context.WithoutCancel(ctx), r)
		}
	}

	if ok && d.model.staleLimits(r) {
		go d.loadLimits(context.WithoutCancel(ctx), r)
	}
}

// typing applies one keypress to the open prompt. Called with the lock held.
func (d *dash) typing(ctx context.Context, k tui.Key) {
	switch {
	case k.Code == tui.KeyEscape:
		d.model.input = prompt{}

	case k.Code == tui.KeyEnter:
		in := d.model.input
		d.model.input = prompt{}

		go d.applyLimits(context.WithoutCancel(ctx), in)

	case k.Code == tui.KeyBackspace:
		if r := []rune(d.model.input.buffer); len(r) > 0 {
			d.model.input.buffer = string(r[:len(r)-1])
		}

	case k.Code == tui.KeyRune && k.Rune >= ' ' && k.Rune != 127:
		// Bounded, because a footer is one line and a buffer that outgrows it would scroll
		// the thing somebody is reading while they type into it.
		if len([]rune(d.model.input.buffer)) < 40 {
			d.model.input.buffer += string(k.Rune)
		}
	}
}

// applyLimits parses what was typed and sets it, saying what happened either way.
//
// The syntax is "cpu,memory": "2,4g" caps both, "2" caps only cpu, ",4g" only memory. Two
// values on one line rather than two prompts in a row, because the pair is one decision and
// asking twice makes cancelling halfway a state somebody can get stuck in.
//
// There is no way to clear a ceiling from here, and that is docker's rule rather than a
// missing feature. In its update API a zero is "leave this alone", not "remove this" - so a
// request to clear one is accepted, changes nothing, and reports success. Refusing it out
// loud beats reporting a change that did not happen, which is what the first version did.
func (d *dash) applyLimits(ctx context.Context, in prompt) {
	lim, ok := d.opt.Provider.(provider.Limiter)
	if !ok {
		d.say("this provider cannot set limits")

		return
	}

	cpu, mem, _ := strings.Cut(in.buffer, ",")

	if strings.TrimSpace(in.buffer) == "none" {
		cpu, mem = "none", "none"
	}

	d.mu.Lock()
	current := d.model.limits
	d.mu.Unlock()

	if asked, half := clearingSomething(cpu, mem, current); asked {
		d.say("docker cannot remove a %s limit from a container that exists - "+
			"recreate the sandbox to clear it", half)

		return
	}

	want, err := provider.ParseLimits(cpu, mem)
	if err != nil {
		d.say("%s", err)

		return
	}

	// An unmentioned half is left alone rather than cleared. Typing "2" to cap the cpu should
	// not silently remove a memory ceiling somebody set earlier - and a zero would not remove
	// it anyway, so sending the current value is both honest and what docker will do.
	if strings.TrimSpace(cpu) == "" {
		want.NanoCPUs = current.NanoCPUs
	}

	if strings.TrimSpace(mem) == "" {
		want.MemBytes = current.MemBytes
	}

	if !want.Capped() {
		d.say("nothing to set - type a value like 0.5,512m")

		return
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := lim.SetLimits(ctx, in.ref, want); err != nil {
		d.say("%s", firstLine(err.Error()))

		return
	}

	// Force the meters to re-read rather than believing what was asked for: what docker
	// accepted is the only thing worth drawing.
	d.mu.Lock()
	d.model.limitsFor = ""
	d.mu.Unlock()

	d.say("%s limited to %s", in.name, describeLimits(want))
}

// clearingSomething reports whether the request would remove a ceiling that is currently set,
// and names which one. Asking to clear something that is already uncapped is not a request to
// clear anything, so it passes.
func clearingSomething(cpu, mem string, current provider.Limits) (bool, string) {
	switch {
	case strings.TrimSpace(cpu) == "none" && current.NanoCPUs > 0:
		return true, "cpu"
	case strings.TrimSpace(mem) == "none" && current.MemBytes > 0:
		return true, "memory"
	}

	return false, ""
}

// describeLimits says a ceiling the way somebody would read it back.
func describeLimits(l provider.Limits) string {
	switch {
	case !l.Capped():
		return "nothing"
	case l.NanoCPUs == 0:
		return humanBytes(l.MemBytes) + " of memory, cpu uncapped"
	case l.MemBytes == 0:
		return trimZeros(float64(l.NanoCPUs)/1e9) + " cores, memory uncapped"
	}

	return fmt.Sprintf("%s cores and %s",
		trimZeros(float64(l.NanoCPUs)/1e9), humanBytes(l.MemBytes))
}

// followLimits re-reads the selected service's ceilings when they can have changed.
//
// Called from the poll rather than only from the arrow keys because the thing that makes them
// appear is usually not a keypress: a service that was asleep when it was selected has no
// container to inspect, and the ceilings only become readable when something wakes it.
func (d *dash) followLimits(ctx context.Context) {
	d.mu.Lock()

	r, ok := d.model.currentRow()
	stale := ok && d.model.staleLimits(r)

	d.mu.Unlock()

	if stale {
		d.loadLimits(ctx, r)
	}
}

// loadLimits asks what one service is allowed. Not every backend can say, and a backend that
// cannot is not an error - the detail block simply has no ceiling to draw.
func (d *dash) loadLimits(ctx context.Context, r row) {
	lim, ok := d.opt.Provider.(provider.Limiter)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	got, err := lim.Limits(ctx, r.Ref)

	d.mu.Lock()
	defer d.mu.Unlock()

	// The selection can move while an inspect is in flight, and one service's ceiling drawn
	// under another's usage is worse than no ceiling at all.
	if cur, ok := d.model.currentRow(); !ok || cur.Ref != r.Ref {
		return
	}

	// A failure records the attempt rather than retrying every tick. An asleep container has
	// nothing to inspect, which is the ordinary case and not worth a round trip a second.
	if err != nil {
		got = provider.Limits{}
	}

	d.model.limits = got
	d.model.limitsFor, d.model.limitsAwake = r.Ref, r.Awake
}

// printOnce is the non-interactive path: the same information, printed and gone.
func printOnce(ctx context.Context, opt Options, out *os.File) error {
	units, err := opt.Provider.List(ctx, "")
	if err != nil {
		return err
	}

	rows := rowsFrom(units)

	if len(rows) == 0 {
		fmt.Fprintln(out, "no sandboxes yet. `sbx init` makes one.")

		return nil
	}

	fmt.Fprintf(out, "%-20s %-14s %-8s %s\n", "SANDBOX", "SERVICE", "STATE", "ADDRESS")

	for _, r := range rows {
		state := "asleep"
		if r.Awake {
			state = "awake"
		}

		fmt.Fprintf(out, "%-20s %-14s %-8s %s\n", r.Sandbox, r.Service, state, r.Address)
	}

	fmt.Fprintln(out, "\nthis is not a terminal, so the live dashboard is not available here")

	return nil
}
