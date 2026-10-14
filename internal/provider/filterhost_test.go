package provider

import (
	"errors"
	"strings"
	"testing"
)

// The refusal has to carry the address it probed and the one-liner that checks it, because the
// reader's next move is to look at their own interfaces. 198.51.100.1 is TEST-NET-2: no machine
// holds it, so the bind fails the way a VM-backed docker's bridge gateway does.
func TestFilterHostNamesTheGatewayAndTheCheck(t *testing.T) {
	err := filterHost("198.51.100.1")
	if !errors.Is(err, ErrFilterNeedsNativeDocker) {
		t.Fatalf("an address this machine cannot bind was not refused: %v", err)
	}

	for _, want := range []string{"198.51.100.1", "ifconfig | grep 198.51.100.1",
		"VM-backed docker", "colima", "Docker Desktop", "native", "credentialProxy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q, so it names neither the cause nor the fix: %v", want, err)
		}
	}

	// And an address this machine does hold is a hosted filter, which is what Linux looks like.
	if err := filterHost("127.0.0.1"); err != nil {
		t.Errorf("loopback was refused, so the probe refuses a native host too: %v", err)
	}
}
