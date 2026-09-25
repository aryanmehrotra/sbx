//go:build unix && !(linux && (amd64 || arm64))

package execd

import (
	"fmt"
	"net"
	"runtime"
)

// listenVsock refuses: execd listens on AF_VSOCK only as the guest agent of a Firecracker
// microVM, and Firecracker runs on linux/amd64 and linux/arm64 alone. Everywhere else the flag
// is a mistake worth naming rather than a listener that silently never comes up.
func listenVsock(port uint32) (net.Listener, error) {
	return nil, fmt.Errorf("%w: --vsock-port %d needs AF_VSOCK, which this execd supports on linux/amd64 and "+
		"linux/arm64 (Firecracker's platforms) and this is %s/%s; drop --vsock-port and serve on --addr",
		errVsockUnsupported, port, runtime.GOOS, runtime.GOARCH)
}
