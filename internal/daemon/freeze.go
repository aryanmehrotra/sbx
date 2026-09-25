package daemon

// Pausing on purpose, for the OpenSandbox API.
//
// The daemon already freezes an idle sandbox that asked for it, and thaws it on the next
// connection - that is sleep with the memory kept. A pause requested through the API is the
// same freeze with one difference: traffic must NOT undo it. So it is a hold on the sandbox
// plus the freeze, and resume is releasing the hold plus an ordinary wake.
//
// It lives in the daemon, not in the API, because the daemon is the one thing that starts and
// stops sandboxes (ARCHITECTURE.md's rule). An API that paused containers behind the daemon's
// back would leave a unit the daemon believes awake, and the reaper and the pause would fight
// over it.

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Refresh runs a discovery pass now rather than on the next tick, so a sandbox created through
// the API is fronted - and its endpoint answers - within the request that is waiting for it.
func (d *daemon) Refresh(ctx context.Context) { d.discover(ctx) }

// Hold marks a sandbox paused on purpose (or releases it) without touching the container. The
// API calls it on start to re-assert pauses that outlived a daemon restart.
func (d *daemon) Hold(sandbox string, held bool) {
	d.heldMu.Lock()
	defer d.heldMu.Unlock()

	if d.held == nil {
		d.held = map[string]bool{}
	}

	if held {
		d.held[sandbox] = true
	} else {
		delete(d.held, sandbox)
	}

	// And on every unit already known, which is what the connection path actually reads.
	d.mu.Lock()
	for _, u := range d.units {
		if u.sandbox == sandbox {
			u.held.Store(held)
		}
	}
	d.mu.Unlock()
}

func (d *daemon) isHeld(sandbox string) bool {
	d.heldMu.RLock()
	defer d.heldMu.RUnlock()

	return d.held[sandbox]
}

// unitsOf returns the units the daemon knows for one sandbox, discovering once if it knows none:
// a sandbox created a moment ago may not have been seen by a tick yet.
func (d *daemon) unitsOf(ctx context.Context, sandbox string) []*unit {
	find := func() []*unit {
		d.mu.Lock()
		defer d.mu.Unlock()

		var out []*unit

		for _, u := range d.units {
			if u.sandbox == sandbox {
				out = append(out, u)
			}
		}

		return out
	}

	if us := find(); len(us) > 0 {
		return us
	}

	d.discover(ctx)

	return find()
}

// Freeze pauses every unit of a sandbox and holds it paused until Thaw.
func (d *daemon) Freeze(ctx context.Context, sandbox string) error {
	pa, err := provider.PauserFor(d.provider)
	if err != nil {
		return err
	}

	us := d.unitsOf(ctx, sandbox)
	if len(us) == 0 {
		return fmt.Errorf("sbx serve has no running service for sandbox %q to pause - it may "+
			"have been removed; `sbx list` shows what exists", sandbox)
	}

	// Held before frozen, so a connection arriving mid-freeze is refused rather than thawing
	// the unit the moment after it stopped.
	d.Hold(sandbox, true)

	for _, u := range us {
		u.held.Store(true) // again: a unit discovered between Hold and here missed the sweep

		if err := u.freeze(ctx, pa); err != nil {
			// The caller answers this with an error and records the sandbox as still running,
			// and resume refuses a sandbox that is not paused. A hold left here would hang up
			// on every connection with no API call able to release it, so undo it. Units
			// already frozen stay frozen but unheld: the next connection thaws them, which is
			// the ordinary wake path.
			d.Hold(sandbox, false)

			return err
		}
	}

	return nil
}

// Thaw releases the hold and wakes the sandbox - from frozen by unpausing, from stopped by
// starting, which is whatever the ordinary wake path would do for a connection.
func (d *daemon) Thaw(ctx context.Context, sandbox string) error {
	d.Hold(sandbox, false)

	for _, u := range d.unitsOf(ctx, sandbox) {
		u.held.Store(false)

		if err := u.wake(ctx, d.provider, d.ready); err != nil {
			// The mirror of Freeze: a failed resume leaves the API saying Paused, so the
			// daemon must keep refusing traffic rather than let a connection wake what the
			// caller still believes is paused.
			d.Hold(sandbox, true)

			return err
		}
	}

	return nil
}

// freeze pauses one unit now, whatever its idle clock says.
func (u *unit) freeze(ctx context.Context, pa provider.Pauser) error {
	u.waking.Lock()
	defer u.waking.Unlock()

	u.mu.Lock()
	for c := range u.live {
		_ = c.Close()
	}

	u.live = map[net.Conn]struct{}{}
	u.mu.Unlock()

	if u.isFrozen() {
		return nil
	}

	if err := pa.Pause(ctx, u.ref); err != nil {
		// A stopped unit has nothing to freeze and is already as paused as it can be: the hold
		// alone keeps it from being started, which is what the caller asked for.
		if strings.Contains(err.Error(), "is not running") {
			u.setAwake(false)
			return nil
		}

		return fmt.Errorf("pausing %s: %w", u.name, err)
	}

	u.setAwake(false)
	u.setFrozen(true)
	logs.Default.Event(logs.LevelInfo, u.sandbox, u.service, "paused", 0, "paused on request")

	return nil
}

// refHeld reports whether the unit behind ref belongs to a sandbox paused through the API, and
// which sandbox that is. d.mu is released before isHeld takes heldMu: Hold takes them in the
// other order.
func (d *daemon) refHeld(ref string) (string, bool) {
	d.mu.Lock()
	u, ok := d.units[ref]
	d.mu.Unlock()

	if !ok {
		return "", false
	}

	return u.sandbox, u.isHeld() || d.isHeld(u.sandbox)
}
