package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// exitSettle is how long after Create a service's container must have stayed up before create
// ticks it. A container that exits at once is still listed running for a moment - measured on
// colima, `sh -c "exit 3"` read as running when create asked straight after Create. A variable so
// tests need not wait it out.
var exitSettle = 500 * time.Millisecond

// exitedAfterCreate refuses a service whose container has exited by the time create would print
// its tick. With a health check the wait above already fails on it; without one, nothing did, and
// "web ✓" was printed for a container that had exited at once - the failure surfaced only in the
// final readiness check, as "could not ask its container".
//
// Only where the runtime records why (provider.ExitReporter) and says it is not running. A
// provider without that record may leave a new service asleep on purpose - a microVM's create
// ends by snapshotting it - and that keeps its tick. Could not ask is not a verdict either.
//
// It watches until exitSettle after made (when Create returned), so a health wait or init that
// already took that long costs nothing more; a container that exits later than that is left to the
// final readiness check, as before.
func exitedAfterCreate(ctx context.Context, p provider.Provider, sandbox, name string, made time.Time) error {
	er, ok := p.(provider.ExitReporter)
	if !ok {
		return nil
	}

	for {
		u, err := unitFor(ctx, p, sandbox, name)
		if err != nil {
			return nil
		}

		if !u.Running {
			st, err := er.ExitOf(ctx, u.Ref)
			if err != nil || st.Status == "running" {
				return nil
			}

			return fmt.Errorf("service %q: its container is not running: %s\n       see why: sbx logs %s %s",
				name, st, sandbox, name)
		}

		if time.Since(made) >= exitSettle {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}
