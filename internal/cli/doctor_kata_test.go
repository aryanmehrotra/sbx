package cli

import (
	"strings"
	"testing"
)

// doctor checks that a runtime is registered with dockerd; it does not run a container in it.
// The row said "✓ isolation kata ... available" on a host where a Kata container got no network
// and could not restart, so it claimed a result nobody measured. It says what was checked, and
// for Kata - a VM per container, which a nested or VM-backed host often cannot give it - that
// working is unverified.
func TestIsolationRowSaysRegisteredNotWorks(t *testing.T) {
	kata := isolationRow("isolation kata", "kata-runtime", "why", true)

	if !kata.Have {
		t.Fatal("a registered runtime must still be reported present")
	}

	if strings.Contains(kata.Detail, "available") || !strings.Contains(kata.Detail, "registered") {
		t.Errorf("detail %q claims more than a registration check shows", kata.Detail)
	}

	if !strings.Contains(kata.Detail, "unverified") || !strings.Contains(kata.Detail, "nested") {
		t.Errorf("detail %q does not say Kata is unverified on a nested or VM host", kata.Detail)
	}

	gv := isolationRow("isolation gvisor", "runsc", "why", true)
	if strings.Contains(gv.Detail, "available") || !strings.Contains(gv.Detail, "registered") {
		t.Errorf("gvisor detail %q claims more than a registration check shows", gv.Detail)
	}

	if absent := isolationRow("isolation kata", "kata-runtime", "why", false); absent.Have || absent.Meaning != "why" {
		t.Errorf("an unregistered runtime = %+v", absent)
	}
}
