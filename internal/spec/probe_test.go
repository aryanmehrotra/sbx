package spec

// How often a service is asked whether it is serving.
//
// The interval was a constant, which is fine for one service and a decision nobody made for
// fourteen: the probes run whether or not anybody is waiting, so a sandbox of fourteen services
// at 300ms is about forty-seven commands a second started inside containers, for ever.

import (
	"strings"
	"testing"
	"time"
)

func TestTheProbeIntervalResolvesServiceThenSandboxThenDefault(t *testing.T) {
	sp := Spec{HealthInterval: "2s"}

	// The service's own wins.
	if got := sp.ProbeInterval(Service{HealthInterval: "500ms"}); got != 500*time.Millisecond {
		t.Errorf("a service's own interval was ignored: %v", got)
	}

	// Else the sandbox's, which is the one somebody turns down for a whole file at once.
	if got := sp.ProbeInterval(Service{}); got != 2*time.Second {
		t.Errorf("the sandbox's default was not used: %v", got)
	}

	// Else the built-in.
	if got := (Spec{}).ProbeInterval(Service{}); got != DefaultHealthInterval {
		t.Errorf("the default was not used: %v", got)
	}
}

// Both ends are refused, because both do something other than what the writer meant.
func TestAnUnusableProbeIntervalIsRefused(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"1ms", "floor"},
		{"10m", "ceiling"},
		{"soon", "not a duration"},
	} {
		err := checkInterval(c.raw)
		if err == nil {
			t.Errorf("%q was accepted", c.raw)

			continue
		}

		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q said %q, want it to mention %q", c.raw, err, c.want)
		}
	}

	for _, ok := range []string{"", "50ms", "300ms", "1s", "5m"} {
		if err := checkInterval(ok); err != nil {
			t.Errorf("%q was refused: %v", ok, err)
		}
	}
}
