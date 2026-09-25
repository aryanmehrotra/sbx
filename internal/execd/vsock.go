//go:build unix

package execd

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// vsockAddr is an AF_VSOCK address. It is defined on every unix so the code that only names it -
// the transport check, error messages - does not need a build tag of its own.
type vsockAddr struct {
	CID  uint32
	Port uint32
}

func (vsockAddr) Network() string { return "vsock" }

func (a vsockAddr) String() string {
	if a.CID == 0xFFFFFFFF {
		return fmt.Sprintf("vsock:any:%d", a.Port)
	}

	return fmt.Sprintf("vsock:%d:%d", a.CID, a.Port)
}

// errVsockUnsupported is --vsock-port on a platform without AF_VSOCK support in execd: a usage
// error, exit 2, not a runtime failure.
var errVsockUnsupported = errors.New("AF_VSOCK is not supported here")

// transportKey marks a request's connection as having arrived over vsock.
type transportKey struct{}

// connContext is the http.Server's ConnContext: it records which transport a connection came
// in on, because the control endpoints (/sbx/seal, /sbx/rekey) answer only on vsock when execd
// is serving it. A vsock conn is recognised by a method, not a type, so this file needs no tag.
func connContext(ctx context.Context, c net.Conn) context.Context {
	if _, ok := c.(interface{ vsockTransport() }); ok {
		return context.WithValue(ctx, transportKey{}, true)
	}

	return ctx
}

// overVsock reports whether the request arrived on the vsock listener.
func overVsock(ctx context.Context) bool {
	v, _ := ctx.Value(transportKey{}).(bool)
	return v
}
