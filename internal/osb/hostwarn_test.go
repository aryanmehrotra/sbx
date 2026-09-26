package osb

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/history"
)

// exposedHost is a provider whose host could not be closed to this sandbox's guests.
type exposedHost struct{ *fakeDocker }

func (exposedHost) HostWarnings(_ context.Context, sandbox string) []string {
	return []string{"the host could not be closed to " + sandbox + "'s guests (iptables: mangle table missing): " +
		"they reach every host service at 10.231.0.1 - see SECURITY.md"}
}

// A guard that could not be installed was said only on the daemon's stderr, which the API's
// caller never sees: their sandbox reported plain Running while it could reach the host. The
// warning now rides on the Running status message and in the sandbox's history.
func TestAHostExposureWarningReachesTheAPICaller(t *testing.T) {
	h := newHarness(t, func(h *harness, o *Options) { o.Provider = exposedHost{h.p} })

	got := h.create(minimalCreate())
	if got.Status.State != stateRunning {
		t.Fatalf("state %s", got.Status.State)
	}

	if !strings.Contains(got.Status.Message, "could not be closed") || !strings.Contains(got.Status.Message, "SECURITY.md") {
		t.Fatalf("Running status message = %q, want the host-exposure warning", got.Status.Message)
	}

	h.srv.wg.Wait()

	recs, err := history.Read(history.Filter{Sandbox: got.ID})
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range recs {
		if r.Event == "warning" && strings.Contains(r.Message, "could not be closed") {
			return
		}
	}

	t.Fatalf("no warning in the sandbox's history: %+v", recs)
}

// And a provider with nothing to warn about leaves Running exactly as it was.
func TestNoHostWarningLeavesRunningPlain(t *testing.T) {
	h := newHarness(t)

	if got := h.create(minimalCreate()); got.Status.State != stateRunning || got.Status.Message != "" {
		t.Fatalf("status = %+v", got.Status)
	}
}
