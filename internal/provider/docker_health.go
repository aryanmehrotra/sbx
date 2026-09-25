package provider

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// healthArgs renders a service's health check as `docker run` flags.
//
// The interval is the floor on how long a wake appears to take, because docker only
// re-evaluates health on it. The long start period is the opposite of low retries: inside it a
// failing check does not latch the container as unhealthy, while a passing one still flips it
// immediately - so a database that needs six seconds to open its data directory is not declared
// broken at 300ms.
//
// startInterval says the engine takes --health-start-interval (API 1.44) and the service asked
// for one: checks then run at that pace inside the start period and at the interval after it.
func healthArgs(svc spec.Service, startInterval bool) []string {
	if svc.Health == "" {
		return nil
	}

	args := []string{
		"--health-cmd", svc.Health,
		"--health-interval", probeInterval(svc).String(),
		"--health-timeout", "2s",
		"--health-retries", "3",
		"--health-start-period", "60s",
	}

	if startInterval {
		if d, err := time.ParseDuration(svc.HealthStartInterval); err == nil && d > 0 {
			args = append(args, "--health-start-interval", d.String())
		}
	}

	return args
}

// startIntervalMajor.startIntervalMinor is the Engine API version that added HealthConfig.StartInterval (Docker 25).
const startIntervalMajor, startIntervalMinor = 1, 44

// startIntervalOnce is endpoint -> *startIntervalProbe: one question per engine per process.
var startIntervalOnce sync.Map

type startIntervalProbe struct {
	once sync.Once
	ok   bool
}

// hasStartInterval asks the engine once per endpoint whether it takes a start interval. An
// engine that cannot be asked is treated as one that does not: the check then runs at its
// steady interval throughout, which is slower to report healthy but never a refused create.
func (d *dockerProvider) hasStartInterval() bool {
	v, _ := startIntervalOnce.LoadOrStore(d.endpoint.String(), &startIntervalProbe{})
	p := v.(*startIntervalProbe)

	p.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var ver struct {
			APIVersion string `json:"ApiVersion"`
		}

		if d.api.do(ctx, "GET", "/version", &ver) == nil {
			p.ok = apiAtLeast(ver.APIVersion, startIntervalMajor, startIntervalMinor)
		}
	})

	return p.ok
}

// apiAtLeast compares a "major.minor" Engine API version.
func apiAtLeast(v string, major, minor int) bool {
	a, b, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}

	ma, err1 := strconv.Atoi(a)
	mi, err2 := strconv.Atoi(b)

	if err1 != nil || err2 != nil {
		return false
	}

	return ma > major || (ma == major && mi >= minor)
}
