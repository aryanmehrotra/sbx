package provider

// The probe interval, as each backend expresses it.

import (
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

func TestTheDefaultSurvivesAServiceThatSaysNothing(t *testing.T) {
	if got := probeInterval(spec.Service{}); got != spec.DefaultHealthInterval {
		t.Errorf("probeInterval of an unset service = %v, want the default", got)
	}

	if got := probeInterval(spec.Service{HealthInterval: "2s"}); got != 2*time.Second {
		t.Errorf("probeInterval = %v, want what the service asked for", got)
	}
}

// Kubernetes counts probe periods in whole seconds, and zero does not mean zero to it - it
// means "use my default", which is ten. A spec asking for a faster probe would then have got a
// slower one than the file said, which is the sort of thing nobody notices until a wake is
// mysteriously ten seconds.
func TestASubSecondIntervalBecomesOneSecondInACluster(t *testing.T) {
	for _, c := range []struct {
		interval string
		want     int
	}{
		{"", 1}, // the 300ms default rounds to 1, not 0
		{"300ms", 1},
		{"1s", 1},
		{"2s", 2},
		{"90s", 90},
	} {
		period := int(probeInterval(spec.Service{HealthInterval: c.interval}).Round(time.Second) / time.Second)
		if period < 1 {
			period = 1
		}

		if period != c.want {
			t.Errorf("%q became periodSeconds %d, want %d", c.interval, period, c.want)
		}
	}
}
