package provider

import (
	"net"
	"strconv"
	"testing"
)

// A slot whose PUBLIC port another program holds is not free, whatever docker says. Two sbx
// daemons on one machine - a live one on the default engine and a throwaway on a second - each
// allocate from slot 0; the second's wake port then cannot bind, and a ping to it lands on the
// first daemon's sandbox instead. Measured: a create's readiness ping woke a sandbox on the
// other engine and the create took 9 s.
func TestSlotWithAHeldPublicPortIsNotFree(t *testing.T) {
	const slot = 57

	d := &dockerProvider{}

	if !d.slotPortsFree(slot) {
		t.Skipf("slot %d's ports are already in use on this machine; the test needs them free", slot)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(publicBase+slot*blockSize))
	if err != nil {
		t.Skipf("could not hold the public port: %v", err)
	}
	defer ln.Close()

	if d.slotPortsFree(slot) {
		t.Fatalf("slot %d reported free while its public port %d is held", slot, publicBase+slot*blockSize)
	}
}

// Every backing port stays below Linux's default ephemeral range (net.ipv4.ip_local_port_range
// starts at 32768), or an outgoing connection on the host can hold a port docker is about to
// publish. Raising maxSlots past this breaks creates at random, not at once.
func TestBackingRangeStaysBelowTheEphemeralPorts(t *testing.T) {
	const linuxEphemeralStart = 32768

	if top := backingBase + maxSlots*blockSize; top > linuxEphemeralStart {
		t.Fatalf("backing ports reach %d, into the ephemeral range at %d - lower maxSlots or move backingBase",
			top, linuxEphemeralStart)
	}

	if top := publicBase + maxSlots*blockSize; top > backingBase {
		t.Fatalf("public ports reach %d, into the backing range at %d", top, backingBase)
	}
}
