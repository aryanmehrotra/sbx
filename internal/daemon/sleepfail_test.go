package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// stopFails is a provider whose Stop fails with err.
type stopFails struct {
	transitionRecorder
	err error
}

func (s *stopFails) Stop(context.Context, string) error { return s.err }

// A sleep the provider refused while leaving the workload up (a microVM whose seal was never
// confirmed, re-keyed and still serving) must leave the unit awake, so the reaper's next tick
// tries again. Recorded as asleep, it would hold its memory until some client happened to wake
// it, and nothing would ever try to sleep it again.
func TestASleepRefusedWithTheWorkloadStillRunningLeavesTheUnitAwake(t *testing.T) {
	log.SetOutput(io.Discard)

	for _, tc := range []struct {
		name  string
		err   error
		awake bool
	}{
		{"still running", fmt.Errorf("sleeping x: seal unconfirmed: %w", provider.ErrStillRunning), true},
		{"stopped", errors.New("sleeping x: stopped, next wake is a cold boot"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &stopFails{err: tc.err}

			u := newUnit("t", "svc", "ref", "inst-ref", "t", nil, true)
			u.setAwake(true)
			u.lastByte.Store(time.Now().Add(-time.Hour).UnixNano())

			u.sleep(context.Background(), p, time.Millisecond)

			if got := u.isAwake(); got != tc.awake {
				t.Fatalf("awake after a failed sleep (%v) = %v, want %v", tc.err, got, tc.awake)
			}
		})
	}
}
