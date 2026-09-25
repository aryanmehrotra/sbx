package provider

import (
	"context"
	"net"
)

// DialFunc opens one connection to one workload port.
type DialFunc func(ctx context.Context) (net.Conn, error)

// GuestDialer is implemented by a provider whose workloads the daemon cannot reach by dialling
// a TCP address - a microVM whose only door from the host is a Firecracker vsock device, which
// is a unix socket plus a handshake (internal/fcvsock).
//
// The daemon asks once per leg, when it starts fronting the unit, with guestPort set to that
// leg's Upstream.Port: a provider implementing this interface decides what Upstream.Port means
// for its own units, and for Firecracker it is the guest port. ok=false means "dial Upstream
// over TCP as usual", so one provider can mix the two - vsock for execd, a tap for the rest.
//
// The returned DialFunc is called on every client connection, so it must be cheap and safe for
// concurrent use, and must honour ctx: a VM that does not answer holds a client for as long as
// the DialFunc allows.
//
// A provider implementing this must also make Start return only once the workload may be
// served, which for a restored snapshot includes re-keying execd (internal/execdctl): the wake
// proxy splices a client the moment Start and the probe say so, and nothing later re-checks.
type GuestDialer interface {
	GuestDialer(sandbox, service string, guestPort int) (dial DialFunc, ok bool)
}
