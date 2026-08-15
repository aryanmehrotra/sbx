package daemon

import (
	"testing"
	"time"
)

// The reaper used to tick every 30s no matter what --idle said, so anything under half a
// minute took two fixed ticks to sleep: a sandbox set to 5s slept after 60.
func TestReapEveryFollowsIdle(t *testing.T) {
	cases := []struct{ idle, want time.Duration }{
		{3 * time.Second, time.Second},       // floor: never busier than once a second
		{30 * time.Second, 10 * time.Second}, // a third of the window
		{5 * time.Minute, 30 * time.Second},  // ceiling: 100 sleeping sandboxes stay cheap
		{time.Hour, 30 * time.Second},
	}

	for _, c := range cases {
		if got := reapEvery(c.idle); got != c.want {
			t.Errorf("reapEvery(%s) = %s, want %s", c.idle, got, c.want)
		}
	}

	// The property that actually matters: a sandbox must be eligible to sleep well inside
	// its own idle window, not several windows later.
	for _, idle := range []time.Duration{time.Second, 3 * time.Second, time.Minute} {
		if reapEvery(idle) > idle {
			t.Errorf("reapEvery(%s) = %s, which is longer than the window itself", idle, reapEvery(idle))
		}
	}
}
