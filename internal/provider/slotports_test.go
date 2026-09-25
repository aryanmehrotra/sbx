package provider

import (
	"net"
	"strconv"
	"testing"
)

// A slot the container list calls free can still be unusable: its PUBLIC ports may be held by
// something outside this engine - on one host, the sbx serve of another docker engine (a second
// colima profile) fronts the same 20000+ block. Handing out that slot creates a sandbox whose
// wake port this daemon can never bind: it is not fronted, the idle reaper never sees it, and a
// client dialling its port reaches the OTHER engine's daemon instead. Measured: that was
// TestDockerOpenSandboxLifecycle failing 4/4 on an isolated engine with "units: []".
func TestASlotWhosePublicPortIsTakenIsNotFree(t *testing.T) {
	d := newDocker(dockerEndpoint{})

	slot := -1

	for s := maxSlots - 1; s >= 0; s-- {
		if d.slotPortsFree(s) {
			slot = s
			break
		}
	}

	if slot < 0 {
		t.Skip("no slot on this machine has free ports to test with")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(publicBase+slot*blockSize))
	if err != nil {
		t.Fatal(err)
	}

	if d.slotPortsFree(slot) {
		_ = ln.Close()
		t.Fatalf("slot %d was called free while its public port is held by another listener", slot)
	}

	_ = ln.Close()

	if !d.slotPortsFree(slot) {
		t.Fatalf("slot %d is not free once the listener is gone", slot)
	}
}
