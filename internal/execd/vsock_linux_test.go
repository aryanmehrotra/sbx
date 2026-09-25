//go:build linux && (amd64 || arm64)

package execd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/aryanmehrotra/sbx/internal/execdctl"
)

// vmaddrCIDLocal is VMADDR_CID_LOCAL: a connection to this VM itself, served by vsock_loopback.
const vmaddrCIDLocal = 1

// dialVsock connects to cid:port. Test-only: the host side of a real deployment is Firecracker's
// unix socket (internal/fcvsock), never AF_VSOCK.
func dialVsock(cid, port uint32) (net.Conn, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	sa := sockaddrVM{Family: afVsock, Port: port, CID: cid}

	if _, _, e := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); e != 0 {
		_ = syscall.Close(fd)
		return nil, e
	}

	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}

	return newVsockConn(fd, vsockAddr{}, vsockAddr{CID: cid, Port: port}), nil
}

// The whole guest path, on this kernel's own vsock: listen with raw syscalls, serve execd's
// handler, reach it through an AF_VSOCK connection. It needs AF_VSOCK and the vsock_loopback
// transport, which a CI runner or a container usually lacks - so it skips, saying which, rather
// than failing for a reason that is not execd's. Inside the Firecracker guest it runs for real.
func TestVsockListenerServesTheAPI(t *testing.T) {
	port := uint32(40000 + os.Getpid()%20000)

	ln, err := listenVsock(port)
	if err != nil {
		if errors.Is(err, syscall.EAFNOSUPPORT) {
			t.Skipf("no AF_VSOCK in this kernel: %v", err)
		}

		t.Fatal(err)
	}
	defer ln.Close()

	s, err := New(Options{AccessToken: "tok", OutputDir: t.TempDir(),
		ControlSecret: "vsock-boot-secret-0123456789abcdef", ControlOverVsockOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	hs := &http.Server{Handler: s, ConnContext: connContext}

	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()

	probe, err := dialVsock(vmaddrCIDLocal, port)
	if err != nil {
		// ENODEV / EHOSTUNREACH / ECONNRESET: no loopback transport (modprobe vsock_loopback).
		t.Skipf("AF_VSOCK exists but cannot reach this VM's own CID (vsock_loopback not loaded?): %v", err)
	}

	_ = probe.Close()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return dialVsock(vmaddrCIDLocal, port)
		},
	}}

	resp, err := client.Get("http://execd/ping")
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ping over vsock: %d", resp.StatusCode)
	}

	// The control endpoints answer here - connContext marked the conn as vsock - and a re-key
	// over it takes effect.
	ctl := execdctl.Client{Dial: func(context.Context) (net.Conn, error) { return dialVsock(vmaddrCIDLocal, port) }}

	if err := ctl.Rekey(context.Background(), "vsock-boot-secret-0123456789abcdef", execdctl.Rekey{
		Generation: 1, AccessToken: "tok2", ControlSecret: "vsock-next-secret-0123456789abcdef",
	}); err != nil {
		t.Fatalf("re-key over vsock: %v", err)
	}

	// Close unblocks an Accept in progress and Serve returns the clean-stop error.
	done := make(chan error, 1)

	go func() { _, err := ln.Accept(); done <- err }()

	time.Sleep(50 * time.Millisecond)
	_ = ln.Close()

	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close: %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not wake a pending Accept")
	}
}
