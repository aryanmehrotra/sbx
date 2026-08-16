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
}

func (d *dash) poll(ctx context.Context) {
	t := time.NewTicker(Refresh)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.refresh(ctx)
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

	events, _ := history.Read(history.Filter{Kind: "event", Limit: 3})

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
	}
}

// handle applies a keypress. It returns true when the user is finished.
func (d *dash) handle(ctx context.Context, k tui.Key) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Any key dismisses the log overlay, which is what the overlay says it does.
	if d.model.logs != nil {
		d.model.logs = nil

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

	switch {
	case k.Code == tui.KeyCtrlC, k.Rune == 'q':
		return true

	case k.Code == tui.KeyUp, k.Rune == 'k':
		d.model.selected = max(0, d.model.selected-1)

	case k.Code == tui.KeyDown, k.Rune == 'j':
		d.model.selected = min(len(d.model.rows)-1, d.model.selected+1)

	case k.Code == tui.KeyEnter:
		go d.wake(context.WithoutCancel(ctx), d.current())

	case k.Rune == 's':
		go d.sleep(context.WithoutCancel(ctx), d.current())

	case k.Rune == 'l':
		go d.showLogs(context.WithoutCancel(ctx), d.current())

	case k.Rune == 'd':
		if r, ok := d.currentRow(); ok {
			d.model.confirm = fmt.Sprintf("remove %s and its data?", r.Sandbox)
		}

	case k.Rune == 'r':
		go d.refresh(context.WithoutCancel(ctx))
	}

	return false
}

func (d *dash) current() row {
	r, _ := d.currentRow()

	return r
}

func (d *dash) currentRow() (row, bool) {
	if d.model.selected < 0 || d.model.selected >= len(d.model.rows) {
		return row{}, false
	}

	return d.model.rows[d.model.selected], true
}

func (d *dash) say(format string, a ...any) {
	d.mu.Lock()
	d.model.message = fmt.Sprintf(format, a...)
	d.mu.Unlock()
}

// wake connects to the service, because connecting is the only way this product wakes
// anything. Calling the provider's Start directly would demonstrate a path no user has.
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

	_ = conn.Close()

	d.say("%s/%s woke in %dms", r.Sandbox, r.Service, time.Since(start).Milliseconds())

	d.refresh(ctx)
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

func (d *dash) showLogs(ctx context.Context, r row) {
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
	d.model.logs = lines
	d.model.logsTitle = r.Sandbox + "/" + r.Service
	d.mu.Unlock()
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
