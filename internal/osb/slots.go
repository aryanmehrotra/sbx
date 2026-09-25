package osb

// Slots for a burst of creates.
//
// The provider's AllocSlot lists every container and holds the machine's slot lock until the
// new container exists, because until then nothing can see the slot is spoken for. For one
// create that is right. For a hundred it is a queue: each waits for every earlier create's
// list and `docker run` in turn, the hundredth for about a minute on colima.
//
// Here the lock covers only the choice. Slots handed out by this server whose containers do
// not exist yet are kept in s.reserved, which the next choice treats as taken - that closes
// the gap in-process, which is where a burst comes from. The list comes from the coalescer, so
// a burst shares a few lists instead of making one each. A reservation outlives its container's
// creation by slotGrace, because a list that began before the container existed may still be
// in someone's hands.
//
// What this gives up: another process (`sbx create` on the CLI) choosing a slot during this
// server's `docker run` can pick the same one, where before it waited for the lock. It then
// fails at `docker run` on the port, as two machines sharing one remote engine always could,
// and a retry takes the next slot (TROUBLESHOOTING.md). The port probe in PickSlot still
// narrows it.

import (
	"context"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

const slotGrace = time.Minute

func (s *Server) createPicked(ctx context.Context, id string, svc spec.Service, picker provider.SlotPicker) error {
	select {
	case s.dockerSem <- struct{}{}:
	case <-ctx.Done():
		return errGone
	}
	defer func() { <-s.dockerSem }()

	units, err := s.lister.units(ctx)
	if err != nil {
		return err
	}

	s.trace.mark(id, "containers listed")

	unlock := s.lockSlots()

	s.slotMu.Lock()

	now := time.Now()
	taken := make(map[int]bool, len(units)+len(s.reserved))

	for slot, until := range s.reserved {
		if !until.IsZero() && now.After(until) {
			delete(s.reserved, slot)
			continue
		}

		taken[slot] = true
	}

	for _, u := range units {
		taken[u.Slot] = true
	}

	slot, err := picker.PickSlot(taken)
	if err == nil {
		s.reserved[slot] = time.Time{} // in flight: no expiry until the container exists
	}
	s.slotMu.Unlock()
	unlock()

	if err != nil {
		return err
	}

	s.trace.mark(id, "slot allocated")

	eps := s.p.Endpoints(id, service, slot, 0, svc.Ports)
	err = s.p.Create(ctx, id, slot, 0, service, svc, eps, "", provider.IsolationContainer)

	s.slotMu.Lock()
	if err != nil {
		delete(s.reserved, slot)
	} else {
		s.reserved[slot] = time.Now().Add(slotGrace)
	}
	s.slotMu.Unlock()

	if err != nil {
		return err
	}

	// A DELETE that arrived while docker was creating found no container and removed only the
	// record. Without this the container would outlive the sandbox that owned it.
	if _, ok := s.snapshot(id); !ok || ctx.Err() != nil {
		_ = s.p.Remove(context.WithoutCancel(ctx), id)
		return errGone
	}

	return nil
}
