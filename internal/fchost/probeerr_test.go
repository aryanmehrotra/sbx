package fchost

import (
	"context"
	"errors"
	"io"
	"testing"
)

// unreachableHelper is the helper VM up (lima says running) but every call into it failing, as
// an ssh hop that times out does.
type unreachableHelper struct{ *loopRunner }

func (unreachableHelper) Run(context.Context, Cmd) error {
	return errors.New("ssh: connect to host 127.0.0.1 port 60022: operation timed out")
}

// A call into the helper VM that fails is a question nobody answered, not a service with no
// health check. Returning declared=false made the CLI's wait return at once and the daemon mark
// the unit awake "unverified", the same false "serving" docker had when its engine stalled.
func TestRemoteProbeThatCannotAskIsNotUndeclared(t *testing.T) {
	lr := &loopRunner{prov: &recProvider{}}
	r := &Remote{M: current(t, &Manager{Driver: lima{}, Config: testCfg, Run: unreachableHelper{lr}, Out: io.Discard, StateDir: t.TempDir()})}

	if serving, declared := r.Probe(t.Context(), "sbx-demo-cache"); serving || !declared {
		t.Errorf("Probe with the helper VM unreachable = (%v, %v), want (false, true)", serving, declared)
	}

	if serving, declared := r.Healthy(t.Context(), "sbx-demo-cache"); serving || !declared {
		t.Errorf("Healthy with the helper VM unreachable = (%v, %v), want (false, true)", serving, declared)
	}
}
