package daemon

import (
	"context"
	"net"
	"syscall"
)

// listenFree binds addr even while no interface holds it (IP_FREEBIND). A microVM bridge is
// made by the first wake after a host reboot, not before, and a filter that could bind only once
// it existed would be missing for the first requests of every such wake - the guest's only
// door, closed for a tick. Bound freely, it is there before the bridge is and answers the moment
// the kernel puts the address on it. Unprivileged: IP_FREEBIND needs no capability.
func listenFree(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_IP, syscall.IP_FREEBIND, 1)
		}); err != nil {
			return err
		}

		return serr
	}}

	return lc.Listen(context.Background(), "tcp", addr)
}
