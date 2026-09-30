package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/snapshotpause"
)

// pauseRunning freezes every running service of a snapshot and returns how to thaw them.
//
// A live database copied while it writes is torn. It was found on ClickHouse, whose background
// merges replace part directories continuously: `cp -a` listed a part, the merge removed it, and
// the copy failed on "can't stat ... No such file or directory". That failure was the lucky
// one - a copy that happened to finish held files from different instants. Paused, the volume
// and the filesystem the commit takes are both one instant, which is what a crash leaves and
// what a database recovers from.
//
// All running services are paused together, for the whole volume phase and the whole commit
// phase, rather than one service at a time. That keeps a service's volume and image from the
// same instant, keeps the order the rollback depends on (every volume before any image), and
// makes the snapshot one instant across services, so an app and its database agree. The cost
// is that the whole sandbox is frozen for the length of the snapshot; connections wait, they
// are not dropped.
//
// Only what is running. A service asleep has nothing in flight and is copied as it is. One
// frozen already (on_idle) stays frozen: this did not pause it, so it does not thaw it.
//
// Each pause is marked with snapshotpause before it happens and unmarked after the thaw, so the
// daemon's discovery does not take it for a pause done outside sbx - see that package.
func pauseRunning(ctx context.Context, p provider.Provider, units []provider.Unit) (resume func() error, err error) {
	pa, ok := p.(provider.Pauser)
	if !ok {
		// A backend that cannot pause has no per-service volume to tear either (a microVM's disk
		// is in its image), so there is nothing to make consistent here.
		return func() error { return nil }, nil
	}

	type held struct {
		ref     string
		release func()
	}

	var paused []held

	resume = func() error {
		var failed []string

		// Background-derived on purpose: a cancelled snapshot must still thaw.
		thawCtx := context.WithoutCancel(ctx)

		for i := len(paused) - 1; i >= 0; i-- {
			h := paused[i]

			if err := pa.Unpause(thawCtx, h.ref); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v - thaw it with: docker unpause %s", h.ref, err, h.ref))
			}

			h.release()
		}

		paused = nil

		if len(failed) > 0 {
			return fmt.Errorf("could not unpause after the snapshot:\n  %s", strings.Join(failed, "\n  "))
		}

		return nil
	}

	for _, u := range units {
		if !u.Running {
			continue
		}

		release := snapshotpause.Hold(u.Ref)

		if err := pa.Pause(ctx, u.Ref); err != nil {
			release()

			return nil, errors.Join(fmt.Errorf("could not pause %s for a consistent copy: %w\n\n"+
				"nothing was saved. A stopped service is copied as it is: sbx sleep %s, then snapshot it",
				u.Service, err, u.Sandbox), resume())
		}

		paused = append(paused, held{u.Ref, release})
	}

	return resume, nil
}

// interrupts catches Ctrl-C and SIGTERM for the length of a snapshot, so the process lives long
// enough to roll back and thaw, and lets each step ask whether it should stop.
type interrupts struct {
	ctx context.Context
	ch  chan os.Signal
	got os.Signal
}

func interruptions(ctx context.Context) *interrupts {
	i := &interrupts{ctx: ctx, ch: make(chan os.Signal, 1)}
	signal.Notify(i.ch, os.Interrupt, syscall.SIGTERM)

	return i
}

// check is nil until the snapshot was interrupted or its context ended, and an error ever after.
func (i *interrupts) check() error {
	if i.got == nil {
		select {
		case s := <-i.ch:
			i.got = s
		default:
		}
	}

	if i.got != nil {
		return fmt.Errorf("interrupted (%v)", i.got)
	}

	return i.ctx.Err()
}

func (i *interrupts) release() { signal.Stop(i.ch) }
