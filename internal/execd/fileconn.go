//go:build unix

package execd

import (
	"errors"
	"io/fs"
	"net"
	"os"
)

// fileConn is a net.Conn over a connected, non-blocking stream socket held as an *os.File: the
// shape of the vsock conn, which the standard library cannot wrap with net.FileConn.
//
// *os.File has Read, Write, Close and poller-backed deadlines already, but its errors are
// *fs.PathError, and *fs.PathError is not a net.Error: it has Timeout() and no Temporary(). That
// difference is load-bearing for net/http. After every response the server aborts its
// background read by setting a read deadline in the past, and then checks the error it gets
// back with err.(net.Error) && ne.Timeout() (net/http/server.go, connReader.backgroundRead). A
// *fs.PathError fails that assertion, so the server takes the expected deadline for a dead
// client and cancels the CONNECTION's context - the parent of every later request's context on
// that keep-alive connection. The first request over a conn is fine; every one after it arrives
// with r.Context() already cancelled.
//
// So Read and Write report errors the way a *net.TCPConn does: as a *net.OpError, which is a
// net.Error, wraps the same cause (errors.Is(err, os.ErrDeadlineExceeded) still holds), and leaves
// io.EOF bare.
type fileConn struct {
	*os.File
	local, remote net.Addr
}

func (c *fileConn) Read(b []byte) (int, error) {
	n, err := c.File.Read(b)
	return n, c.opError("read", err)
}

func (c *fileConn) Write(b []byte) (int, error) {
	n, err := c.File.Write(b)
	return n, c.opError("write", err)
}

func (c *fileConn) LocalAddr() net.Addr  { return c.local }
func (c *fileConn) RemoteAddr() net.Addr { return c.remote }

// opError turns an *os.File error into the *net.OpError a socket conn would have returned. nil
// and io.EOF come through untouched: os.File never wraps them, and callers compare io.EOF by
// identity.
func (c *fileConn) opError(op string, err error) error {
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return err
	}

	cause := pe.Err
	if errors.Is(cause, os.ErrClosed) {
		cause = net.ErrClosed
	}

	return &net.OpError{Op: op, Net: c.local.Network(), Source: c.local, Addr: c.remote, Err: cause}
}
