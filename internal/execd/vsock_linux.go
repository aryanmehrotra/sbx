//go:build linux && (amd64 || arm64)

package execd

// AF_VSOCK, for execd as the guest agent of a Firecracker microVM.
//
// Raw syscalls rather than golang.org/x/sys: the root module has no requires, and the whole of
// what is needed is socket, bind, listen and accept4 with one 16-byte sockaddr. The standard
// library cannot do it through net.FileListener or net.FileConn, both of which reject a socket
// family they do not know, and syscall.Accept4 closes the accepted fd when it cannot parse the
// peer address - which, for AF_VSOCK, it never can.
//
// Built for amd64 and arm64 only: those are the architectures Firecracker runs on, and the only
// ones where SYS_BIND and SYS_ACCEPT4 exist as plain syscalls (386 multiplexes them through
// socketcall).

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	afVsock      = 40         // AF_VSOCK
	vmaddrCIDAny = 0xFFFFFFFF // VMADDR_CID_ANY: accept on whatever CID this VM was given
)

// sockaddrVM is struct sockaddr_vm from <linux/vm_sockets.h>: 16 bytes.
type sockaddrVM struct {
	Family   uint16
	Reserved uint16
	Port     uint32
	CID      uint32
	Flags    uint8
	Zero     [3]uint8
}

// listenVsock listens on an AF_VSOCK stream port, any CID.
func listenVsock(port uint32) (net.Listener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.EAFNOSUPPORT) {
			return nil, fmt.Errorf("vsock port %d: this kernel has no AF_VSOCK (CONFIG_VSOCKETS and a virtio-vsock "+
				"device are needed - Firecracker's CI kernel has both); use --addr instead: %w", port, err)
		}

		return nil, fmt.Errorf("vsock port %d: socket: %w", port, err)
	}

	sa := sockaddrVM{Family: afVsock, Port: port, CID: vmaddrCIDAny}

	if _, _, e := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); e != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("vsock port %d: bind: %w; pick a free port with --vsock-port", port, e)
	}

	if err := syscall.Listen(fd, 1024); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("vsock port %d: listen: %w", port, err)
	}

	// Non-blocking, so os.NewFile registers it with the runtime poller: Accept parks the
	// goroutine instead of a thread, and Close wakes an Accept in progress.
	f := os.NewFile(uintptr(fd), "vsock:"+strconv.FormatUint(uint64(port), 10))

	rc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	return &vsockListener{f: f, rc: rc, addr: vsockAddr{CID: vmaddrCIDAny, Port: port}}, nil
}

type vsockListener struct {
	f      *os.File
	rc     syscall.RawConn
	addr   vsockAddr
	closed atomic.Bool
}

func (l *vsockListener) Addr() net.Addr { return l.addr }

func (l *vsockListener) Close() error {
	l.closed.Store(true)

	return l.f.Close()
}

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
		var (
			nfd  int
			peer sockaddrVM
			aerr error
		)

		err := l.rc.Read(func(fd uintptr) bool {
			n := uint32(unsafe.Sizeof(peer))

			r, _, e := syscall.Syscall6(syscall.SYS_ACCEPT4, fd,
				uintptr(unsafe.Pointer(&peer)), uintptr(unsafe.Pointer(&n)),
				syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0, 0)
			if e == syscall.EAGAIN {
				return false // not ready: park on the poller and try again when it is
			}

			if e != 0 {
				aerr = e
			} else {
				nfd = int(r)
			}

			return true
		})
		if err != nil {
			// The listener was closed: the one error http.Server treats as a clean stop. An Accept
			// parked on the poller when Close runs gets internal/poll's "use of closed file",
			// which is not os.ErrClosed - so the listener's own flag decides (found on a real
			// kernel: the test had only ever skipped).
			if l.closed.Load() || errors.Is(err, os.ErrClosed) {
				return nil, net.ErrClosed
			}

			return nil, err
		}

		// A peer that went away between the handshake and our accept, or a signal: neither is
		// a reason to stop serving.
		if errors.Is(aerr, syscall.ECONNABORTED) || errors.Is(aerr, syscall.EINTR) {
			continue
		}

		if aerr != nil {
			return nil, fmt.Errorf("vsock accept: %w", aerr)
		}

		return newVsockConn(nfd, l.addr, vsockAddr{CID: peer.CID, Port: peer.Port}), nil
	}
}

// vsockConn is an accepted vsock stream: a fileConn - *os.File over the non-blocking fd, for
// Close and deadlines through the poller, with Read and Write errors reported as net.Errors so
// net/http does not cancel the context of every request after a keep-alive connection's first
// (see fileConn).
type vsockConn struct {
	fileConn
}

func newVsockConn(fd int, local, remote vsockAddr) *vsockConn {
	return &vsockConn{fileConn{File: os.NewFile(uintptr(fd), "vsock-conn"), local: local, remote: remote}}
}

// vsockTransport marks this conn as having arrived over vsock; see controlTransport.
func (c *vsockConn) vsockTransport() {}

// CloseWrite is a half-close, so a client that says "that is all I am sending" is heard.
func (c *vsockConn) CloseWrite() error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}

	var serr error

	if err := rc.Control(func(fd uintptr) { serr = syscall.Shutdown(int(fd), syscall.SHUT_WR) }); err != nil {
		return err
	}

	return serr
}
