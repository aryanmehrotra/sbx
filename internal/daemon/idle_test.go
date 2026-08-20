package daemon

import (
	"testing"
	"time"
)

// The per-service idle override is what keeps an agent box awake while work runs inside it with
// no traffic through the proxy. "never"/"0" must map to keep-awake, a duration to a window, and
// anything unparseable to the global default rather than an accidental never-sleep.
func TestIdlePolicy(t *testing.T) {
	cases := []struct {
		in   string
		keep bool
		d    time.Duration
	}{
		{"", false, 0},
		{"never", true, 0},
		{"0", true, 0},
		{"30m", false, 30 * time.Minute},
		{"90s", false, 90 * time.Second},
		{"garbage", false, 0},
	}

	for _, c := range cases {
		keep, d := idlePolicy(c.in)
		if keep != c.keep || d != c.d {
			t.Errorf("idlePolicy(%q) = %v,%v want %v,%v", c.in, keep, d, c.keep, c.d)
		}
	}
}
