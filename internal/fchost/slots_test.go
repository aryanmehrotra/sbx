package fchost

import (
	"strings"
	"testing"
)

// Every other test in the package sees a host that holds nothing, not whatever this machine runs.
func init() { hostBusySlots = func() string { return "" } }

func TestGuestArgvCarriesTheSlotsTheHostHolds(t *testing.T) {
	saved := hostBusySlots
	t.Cleanup(func() { hostBusySlots = saved })

	hostBusySlots = func() string { return "0,3" }

	got := strings.Join(GuestArgv("create", []string{"x"}), " ")
	if got != "env SBX_PROVIDER_KIND=firecracker SBX_FC_HOST_BUSY_SLOTS=0,3 /usr/local/bin/sbx create x" {
		t.Fatalf("argv = %s", got)
	}

	hostBusySlots = func() string { return "" }

	if got := strings.Join(GuestArgv("list", nil), " "); strings.Contains(got, "BUSY") {
		t.Fatalf("argv = %s: nothing held, nothing sent", got)
	}
}

func TestBusySlots(t *testing.T) {
	held := map[int]bool{0: true, 2: true, 127: true}

	if got := busySlots(func(s int) bool { return !held[s] }); got != "0,2,127" {
		t.Fatalf("busySlots = %q", got)
	}
}
