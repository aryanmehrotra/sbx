package fc

import "testing"

func TestNewGuestIsVsockOnLinux(t *testing.T) {
	g := NewGuest()
	if _, ok := g.(VsockGuest); !ok || !g.Available() {
		t.Fatalf("NewGuest() = %T (available %v), want VsockGuest: linux is where the provider runs VMs", g, g.Available())
	}
}
