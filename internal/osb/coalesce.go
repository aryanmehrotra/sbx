package osb

// Many callers, one docker call.
//
// A burst of creates is followed by a burst of GETs - every SDK polls its sandbox until Running -
// and each GET asked docker for the container list. A hundred list calls at once against a
// VM-backed engine is not a hundred times one: they queue behind each other in the socket
// forward and in dockerd, and the last GET waited for all of them. Coalesced, the callers that
// arrive while a list is in flight share the next one.
//
// The next one, not the one in flight: a list that started before a caller arrived may predate
// the container that caller is asking about, so it is never handed out. Every caller gets a
// result that began after it asked, which is exactly the freshness an uncoalesced call had.

import (
	"context"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

type listCall struct {
	done  chan struct{}
	units []provider.Unit
	err   error
}

type listCoalescer struct {
	list func(ctx context.Context) ([]provider.Unit, error)

	mu      sync.Mutex
	next    *listCall
	running bool
}

// units returns the provider's full list, from a call that started after this one was made.
// The slice is shared between callers and must not be modified.
func (c *listCoalescer) units(ctx context.Context) ([]provider.Unit, error) {
	c.mu.Lock()

	if c.next == nil {
		c.next = &listCall{done: make(chan struct{})}
	}

	call := c.next

	if !c.running {
		c.running = true

		go c.loop()
	}
	c.mu.Unlock()

	select {
	case <-call.done:
		return call.units, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// loop runs one list per batch of waiters until nobody is waiting. Its context is its own: a
// batch serves many requests, and the first of them hanging up must not fail the rest.
func (c *listCoalescer) loop() {
	for {
		c.mu.Lock()

		call := c.next
		c.next = nil

		if call == nil {
			c.running = false
			c.mu.Unlock()

			return
		}
		c.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		call.units, call.err = c.list(ctx)
		cancel()

		close(call.done)
	}
}
