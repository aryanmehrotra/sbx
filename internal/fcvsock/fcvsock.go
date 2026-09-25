// Package fcvsock dials a port inside a Firecracker microVM from the host.
//
// Firecracker's vsock device is "hybrid": the guest sees a real AF_VSOCK socket, the host sees
// a unix socket at the device's uds_path. A host connection names the guest port it wants with
// one line, "CONNECT <port>\n", and Firecracker answers "OK <host port>\n" once the guest has
// accepted - or closes the socket if nothing in the guest is listening on that port. After the
// OK line the unix socket is the guest stream, byte for byte.
//
// So the host needs no AF_VSOCK, no kernel module and no privileges, only this handshake; and a
// wake proxy that splices bytes can splice these exactly as it splices a TCP connection.
// Measured on a restored VM (docs/superpowers/specs/2026-09-26-firecracker-spike.md): dial +
// CONNECT + OK is 4.1 ms median under nested virtualisation.
package fcvsock

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

// DefaultTimeout bounds connect plus handshake when neither the Dialer nor the caller's context
// says otherwise. A restored VM answers in single-digit milliseconds; seconds mean the VM is
// paused, wedged or gone, and holding a client longer than that only hides which.
const DefaultTimeout = 5 * time.Second

// maxReply caps the handshake line. "OK 4294967295\n" is 14 bytes; anything much longer is not
// Firecracker, and reading it unbounded would let a broken peer grow our buffer forever.
const maxReply = 64

var (
	// ErrNoVM is a uds_path nobody is listening on: the VM is not running, or the path is not
	// this sandbox's vsock device (a clone restored with vsock_override has its own path).
	ErrNoVM = errors.New("no Firecracker vsock device at this path")

	// ErrRefused is Firecracker hanging up instead of answering OK, which it does when the guest
	// has no listener on the port - or a reply that is not the protocol at all.
	ErrRefused = errors.New("guest refused the vsock connection")

	// ErrTimeout is a connect or handshake that did not finish within the timeout.
	ErrTimeout = errors.New("vsock handshake timed out")
)

// Dialer reaches one guest port through a Firecracker vsock device. The zero Timeout means
// DefaultTimeout; a deadline on the caller's context applies as well, whichever is sooner.
type Dialer struct {
	// UDSPath is the device's uds_path - for a restored clone, its vsock_override.uds_path.
	UDSPath string

	// Port is the guest's AF_VSOCK port: for execd, the --vsock-port it was started with.
	Port uint32

	Timeout time.Duration
}

// Dial is Dialer{UDSPath: path, Port: port}.DialContext(ctx).
func Dial(ctx context.Context, path string, port uint32) (net.Conn, error) {
	return Dialer{UDSPath: path, Port: port}.DialContext(ctx)
}

// Conn is an established guest stream: the unix connection underneath, plus any bytes the guest
// sent that arrived in the same read as the OK line. Write, Close, CloseWrite and the deadlines
// are the unix connection's, so a proxy's half-close reaches the guest.
type Conn struct {
	*net.UnixConn

	// HostPort is the number Firecracker reported in its OK line: the host-side port it
	// allocated for this connection. It tells connections apart in logs; nothing else uses it.
	HostPort uint32

	r io.Reader // what followed the OK line in the handshake read, then the conn itself
}

func (c *Conn) Read(b []byte) (int, error) { return c.r.Read(b) }

// DialContext connects to the device's unix socket, performs the handshake and returns the
// stream. Every failure says which of three things went wrong - no VM, no listener in the
// guest, or no answer in time - because each has a different fix.
func (d Dialer) DialContext(ctx context.Context) (net.Conn, error) {
	if d.UDSPath == "" {
		return nil, errors.New("fcvsock: UDSPath is empty; pass the vsock device's uds_path for this sandbox")
	}

	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	// Our own timeout carries a cause, so a caller who cancelled gets context.Canceled back
	// and not a timeout they never set.
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, ErrTimeout)
	defer cancel()

	var nd net.Dialer

	raw, err := nd.DialContext(ctx, "unix", d.UDSPath)
	if err != nil {
		return nil, d.wrap(ctx, err)
	}

	uc := raw.(*net.UnixConn)

	c, err := d.handshake(ctx, uc)
	if err != nil {
		_ = uc.Close()
		return nil, d.wrap(ctx, err)
	}

	return c, nil
}

// wrap turns a low-level failure into the error that names its fix.
func (d Dialer) wrap(ctx context.Context, err error) error {
	if errors.Is(err, ErrRefused) {
		return err
	}

	if ctx.Err() != nil {
		if errors.Is(context.Cause(ctx), ErrTimeout) {
			return d.timeoutErr()
		}

		return ctx.Err()
	}

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return d.timeoutErr()
	}

	// ENOENT: no socket file. ECONNREFUSED: a stale file whose firecracker process is gone.
	// Both mean there is no VM behind this path right now.
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%w: %s (%v); start or restore the VM, or check the sandbox's vsock uds_path",
			ErrNoVM, d.UDSPath, err)
	}

	return fmt.Errorf("fcvsock: %s port %d: %w", d.UDSPath, d.Port, err)
}

func (d Dialer) timeoutErr() error {
	return fmt.Errorf("%w: %s port %d gave no answer; the VM may be paused, still restoring, or wedged",
		ErrTimeout, d.UDSPath, d.Port)
}

func (d Dialer) handshake(ctx context.Context, uc *net.UnixConn) (*Conn, error) {
	// The context bounds the handshake through the socket's deadline, so a cancel unblocks a
	// read already in progress rather than waiting for it to end on its own.
	if dl, ok := ctx.Deadline(); ok {
		_ = uc.SetDeadline(dl)
	}

	stop := context.AfterFunc(ctx, func() { _ = uc.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if _, err := fmt.Fprintf(uc, "CONNECT %d\n", d.Port); err != nil {
		return nil, err
	}

	// Buffered, so the reply costs one read rather than one per byte. Whatever the guest sent
	// after the newline stays in br and is handed back through Conn.Read.
	br := bufio.NewReaderSize(uc, maxReply)

	line, err := br.ReadSlice('\n')
	if err != nil {
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET):
			return nil, fmt.Errorf("%w: port %d via %s - nothing in the guest accepted it; "+
				"is execd running there with --vsock-port %d?", ErrRefused, d.Port, d.UDSPath, d.Port)
		case errors.Is(err, bufio.ErrBufferFull):
			return nil, fmt.Errorf("%w: port %d via %s answered %q..., which is not a Firecracker handshake reply",
				ErrRefused, d.Port, d.UDSPath, line[:16])
		default:
			return nil, err
		}
	}

	hostPort, ok := parseOK(line)
	if !ok {
		return nil, fmt.Errorf("%w: port %d via %s answered %q, want \"OK <port>\"",
			ErrRefused, d.Port, d.UDSPath, bytes.TrimSpace(line))
	}

	// From here the conn is the caller's: a cancel after this point must not cut it.
	if !stop() {
		return nil, ctx.Err()
	}

	// A stream, not a request: the handshake's deadline must not outlive the handshake, or a
	// quiet session would be cut at the dial timeout.
	if err := uc.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}

	c := &Conn{UnixConn: uc, HostPort: hostPort, r: uc}

	if n := br.Buffered(); n > 0 {
		pre, _ := br.Peek(n)
		c.r = io.MultiReader(bytes.NewReader(bytes.Clone(pre)), uc)
	}

	return c, nil
}

func parseOK(line []byte) (uint32, bool) {
	rest, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("OK "))
	if !ok {
		return 0, false
	}

	n, err := strconv.ParseUint(string(rest), 10, 32)
	if err != nil {
		return 0, false
	}

	return uint32(n), true
}
