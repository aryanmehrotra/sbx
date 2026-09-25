package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Fifty concurrent refreshes cost a few passes, and every caller returns only after a pass that
// began after it asked.
func TestRefreshCoalescesButNeverReturnsAStalePass(t *testing.T) {
	var r refresher

	var passes, started atomic.Int64

	pass := func(context.Context) {
		passes.Add(1)
		started.Add(1)
		time.Sleep(20 * time.Millisecond)
	}

	var wg sync.WaitGroup

	for range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			before := started.Load()
			r.do(context.Background(), pass)

			if started.Load() <= before {
				t.Error("returned without a pass having started after the call")
			}
		}()
	}

	wg.Wait()

	if n := passes.Load(); n > 5 {
		t.Fatalf("%d passes for 50 concurrent refreshes, want a few", n)
	}
}
