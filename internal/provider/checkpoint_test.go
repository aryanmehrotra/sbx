package provider

import "testing"

// The one branch in checkpoint that changes behaviour is podman vs docker: podman's CRIU restore
// works and docker's does not, so a misdetection means a checkpoint that cannot be resumed. The
// socket name is the fast, hermetic half of the detection - assert it here (the version-string
// fallback needs a live runtime and is covered by the end-to-end test on Linux, not a unit test).
func TestIsPodmanDetectsThePodmanSocket(t *testing.T) {
	podman := []string{
		"/run/podman/podman.sock",
		"/Users/x/.local/share/containers/podman/machine/podman.sock",
	}
	for _, addr := range podman {
		d := &dockerProvider{endpoint: dockerEndpoint{Network: "unix", Address: addr}}
		if !d.isPodman() {
			t.Errorf("%q should be detected as podman - checkpoint would take docker's broken path", addr)
		}
	}
}
