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

// The same through the slot-picker path, which is the one the real docker provider takes and
// which returns before the lock-and-list path does. Setting the label after that branch left
// every burst and pool create unlabelled while the test above still passed.
func TestPickedSlotCreatesAreLabelledToo(t *testing.T) {
	pd := &pickingDocker{fakeDocker: newFakeDocker()}

	h := newHarness(t, func(_ *harness, o *Options) {
		o.Provider = pd
		o.Owner = "sbx-serve:test"
	})

	sb := h.create(minimalCreate())
	h.waitState(sb.ID, stateRunning)

	if got := pd.service(sb.ID).OSBOwner; got != "sbx-serve:test" {
		t.Fatalf("a picked-slot create was labelled %q, want sbx-serve:test", got)
	}
}
