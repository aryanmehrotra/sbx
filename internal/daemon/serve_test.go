package daemon

import (
	"testing"
	"time"
)

// How often a never-served unit's health is asked follows its window - the cadence the whole
// reaper ran at before it ticked every second, kept for the one provider call a tick can make.
func TestHealthEveryFollowsIdle(t *testing.T) {
	cases := []struct{ idle, want time.Duration }{
		{3 * time.Second, time.Second},       // floor: never busier than once a second
		{30 * time.Second, 10 * time.Second}, // a third of the window
		{5 * time.Minute, 30 * time.Second},  // ceiling: 100 sleeping sandboxes stay cheap
		{time.Hour, 30 * time.Second},
	}

	for _, c := range cases {
		if got := healthEvery(c.idle); got != c.want {
			t.Errorf("healthEvery(%s) = %s, want %s", c.idle, got, c.want)
		}
	}

	// The property that actually matters: a sandbox must be eligible to sleep well inside
	// its own idle window, not several windows later.
	for _, idle := range []time.Duration{time.Second, 3 * time.Second, time.Minute} {
		if healthEvery(idle) > idle {
			t.Errorf("healthEvery(%s) = %s, which is longer than the window itself", idle, healthEvery(idle))
		}
	}
}
