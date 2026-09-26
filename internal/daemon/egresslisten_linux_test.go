package daemon

import (
	"net"
	"testing"
)

// The filter for a microVM bridge must be up before the bridge is: after a host reboot the
// bridge is made by the first wake, and the guest's first request goes straight to the gateway.
func TestTheFilterBindsAGatewayNoInterfaceHoldsYet(t *testing.T) {
	const addr = "10.231.253.1:0" // slot 253: no sandbox this test could collide with

	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
		t.Skip("10.231.253.1 is already on an interface here; nothing to prove")
	}

	ln, err := listenFree(addr)
	if err != nil {
		t.Fatalf("listenFree(%s) = %v; the filter would be missing until the bridge exists", addr, err)
	}

	ln.Close()
}
