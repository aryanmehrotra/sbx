package osb

import "testing"

// Every container the API creates carries sbx.osb, which is what tells a daemon that does not
// serve the API to leave it alone.
func TestCreatedContainersAreLabelledAsTheAPIs(t *testing.T) {
	h := newHarness(t, func(_ *harness, o *Options) { o.Owner = "sbx-serve:test" })

	sb := h.create(minimalCreate())
	h.waitState(sb.ID, stateRunning)

	if got := h.p.service(sb.ID).OSBOwner; got != "sbx-serve:test" {
		t.Fatalf("the container was created with OSBOwner %q, want sbx-serve:test", got)
	}
}
