package provider

import (
	"slices"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// A health check is a runc exec per interval per container, so the steady interval is what a
// machine of API sandboxes pays; the start interval is how soon the first healthy report lands.
func TestHealthArgsUseTheStartIntervalOnlyWhereTheEngineHasIt(t *testing.T) {
	svc := spec.Service{Health: "check", HealthInterval: "60s", HealthStartInterval: "1s"}

	got := strings.Join(healthArgs(svc, true), " ")
	for _, want := range []string{"--health-interval 1m0s", "--health-start-interval 1s", "--health-start-period 60s"} {
		if !strings.Contains(got, want) {
			t.Errorf("new engine: %q lacks %q", got, want)
		}
	}

	old := healthArgs(svc, false)
	if slices.Contains(old, "--health-start-interval") {
		t.Errorf("an engine before API 1.44 rejects --health-start-interval: %v", old)
	}

	if plain := healthArgs(spec.Service{Health: "check"}, true); slices.Contains(plain, "--health-start-interval") {
		t.Errorf("no start interval asked for, one given: %v", plain)
	}

	if none := healthArgs(spec.Service{}, true); len(none) != 0 {
		t.Errorf("no health command, flags given: %v", none)
	}
}

func TestAPIAtLeast(t *testing.T) {
	cases := map[string]bool{"1.44": true, "1.53": true, "1.43": false, "1.9": false, "2.0": true, "": false, "x": false}
	for v, want := range cases {
		if got := apiAtLeast(v, 1, 44); got != want {
			t.Errorf("apiAtLeast(%q, 1.44) = %v, want %v", v, got, want)
		}
	}
}
