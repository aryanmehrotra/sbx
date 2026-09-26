package provider

import (
	"testing"
	"time"
)

// A create holds volMu from the check that a volume is attached nowhere until the save that
// attaches it. RemoveVolume and CreateVolume checked and acted without it, so a remove could
// delete a volume a create had just checked and was about to attach, and two creates of one name
// could both pass the "exists" check and write over each other's image.
func TestVolumeCreateAndRemoveWaitForAnAttachInProgress(t *testing.T) {
	r := newRig(t)
	t.Setenv("SBX_FC_VOLUME_SIZE", "16m")

	if err := r.p.CreateVolume(r.ctx, "sbx-osb-pvc-a", nil); err != nil {
		t.Fatal(err)
	}

	for name, op := range map[string]func() error{
		"remove": func() error { return r.p.RemoveVolume(r.ctx, "sbx-osb-pvc-a") },
		"create": func() error { return r.p.CreateVolume(r.ctx, "sbx-osb-pvc-b", nil) },
	} {
		r.p.volMu.Lock() // a VM create between its volume check and its save

		done := make(chan error, 1)
		go func() { done <- op() }()

		select {
		case <-done:
			r.p.volMu.Unlock()
			t.Fatalf("%s went ahead while a create was attaching volumes", name)
		case <-time.After(100 * time.Millisecond):
		}

		r.p.volMu.Unlock()

		if err := <-done; err != nil {
			t.Fatalf("%s after the attach: %v", name, err)
		}
	}
}
