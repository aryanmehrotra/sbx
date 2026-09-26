package daemon

import (
	"os"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// The OpenSandbox API hands microVMs to callers it does not otherwise trust; with the jailer off
// each one's VMM is unconfined root. So it is refused unless the operator says, by name, that
// they accept that.
func TestOSBOnFirecrackerWithTheJailerOffIsRefused(t *testing.T) {
	t.Setenv(fc.JailerEnv, "off")

	err := refuseOSBWithoutJailer("firecracker", "127.0.0.1:8080", false)
	if err == nil || !strings.Contains(err.Error(), "--osb-insecure-no-jailer") || !strings.Contains(err.Error(), fc.JailerEnv) {
		t.Fatalf("jailer off: %v", err)
	}

	if err := refuseOSBWithoutJailer("firecracker", "127.0.0.1:8080", true); err != nil {
		t.Fatalf("with --osb-insecure-no-jailer: %v", err)
	}

	for _, ok := range [][2]string{{"docker", "127.0.0.1:8080"}, {"firecracker", ""}} {
		if err := refuseOSBWithoutJailer(ok[0], ok[1], false); err != nil {
			t.Fatalf("%v: %v", ok, err)
		}
	}

	t.Setenv(fc.JailerEnv, "on")

	if err := refuseOSBWithoutJailer("fc", "127.0.0.1:8080", false); err != nil {
		t.Fatalf("jailer on: %v", err)
	}

	t.Setenv(fc.JailerEnv, "maybe")

	if err := refuseOSBWithoutJailer("fc", "127.0.0.1:8080", false); err == nil {
		t.Fatal("an unreadable SBX_FC_JAILER was served")
	}
}

func TestFCFirewallFlagReachesTheProvider(t *testing.T) {
	t.Setenv(fc.FirewallEnv, "")

	if err := applyFCFirewall("unmanaged"); err != nil || os.Getenv(fc.FirewallEnv) != "unmanaged" {
		t.Fatalf("unmanaged: %v, env %q", err, os.Getenv(fc.FirewallEnv))
	}

	if err := applyFCFirewall("off"); err == nil || !strings.Contains(err.Error(), "--fc-firewall") {
		t.Fatalf("an unknown mode: %v", err)
	}
}
