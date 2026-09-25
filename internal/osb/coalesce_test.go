package osb

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Fifty concurrent callers cost a handful of lists, not fifty, and none of them is handed a list
// that started before it asked.
func TestListCoalescerSharesCallsButNeverAStaleOne(t *testing.T) {
	var calls atomic.Int32

	var gen atomic.Int64 // bumped at the start of every list

	c := &listCoalescer{list: func(context.Context) ([]provider.Unit, error) {
		calls.Add(1)
		g := gen.Add(1)
		time.Sleep(20 * time.Millisecond)

		return []provider.Unit{{Slot: int(g)}}, nil
	}}

	var wg sync.WaitGroup

	for range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			before := gen.Load()

			us, err := c.units(context.Background())
			if err != nil || len(us) != 1 {
				t.Errorf("units = %v, %v", us, err)
				return
			}

			if int64(us[0].Slot) <= before {
				t.Errorf("handed list %d, which started before this caller asked (at %d)", us[0].Slot, before)
			}
		}()
	}

	wg.Wait()

	if n := calls.Load(); n > 5 {
		t.Fatalf("%d lists for 50 concurrent callers, want a few", n)
	}
}
