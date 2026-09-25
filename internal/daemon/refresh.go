package daemon

// Refresh, coalesced.
//
// The OpenSandbox API asks for a discovery pass after every create, so the new sandbox is
// fronted within the request waiting for it. A burst of creates asked for a burst of passes, and
// discover is serialised: a hundred creates meant a hundred container lists back to back, each
// slower than the last as the engine filled, and the hundredth create waited for all of them.
//
// Callers that arrive while a pass is running share the next one. The next, never the current:
// a pass that started before a caller's container existed cannot have seen it, so handing its
// result to that caller would be the bug this exists to prevent - a sandbox reported ready
// whose port nobody is listening on.

import (
	"context"
	"sync"
)

type refreshCall struct{ done chan struct{} }

type refresher struct {
	mu      sync.Mutex
	next    *refreshCall
	running bool
}

// do runs pass, or waits for a run of it that starts after this call. ctx bounds only the wait.
func (r *refresher) do(ctx context.Context, pass func(context.Context)) {
	r.mu.Lock()

	if r.next == nil {
		r.next = &refreshCall{done: make(chan struct{})}
	}

	call := r.next

	if !r.running {
		r.running = true

		// Not cancelled with this caller: the pass serves every caller in its batch, and
		// discover hangs the new sandboxes' listeners off the context it is given.
		go r.loop(context.WithoutCancel(ctx), pass)
	}
	r.mu.Unlock()

	select {
	case <-call.done:
	case <-ctx.Done():
	}
}

func (r *refresher) loop(ctx context.Context, pass func(context.Context)) {
	for {
		r.mu.Lock()

		call := r.next
		r.next = nil

		if call == nil {
			r.running = false
			r.mu.Unlock()

			return
		}
		r.mu.Unlock()

		pass(ctx)
		close(call.done)
	}
}
