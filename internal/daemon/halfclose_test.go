package daemon

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns two ends of a real TCP connection, because net.Pipe has no half-close and a
// half-close is the whole subject here.
func tcpPair(t *testing.T) (server, client *net.TCPConn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type accepted struct {
		c   net.Conn
		err error
	}

	got := make(chan accepted, 1)

	go func() {
		c, err := ln.Accept()
		got <- accepted{c, err}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	a := <-got
	if a.err != nil {
		t.Fatal(a.err)
	}

	t.Cleanup(func() { _ = a.c.Close(); _ = c.Close() })

	return a.c.(*net.TCPConn), c.(*net.TCPConn)
}

// A client that half-closes must still get its answer through the tunnel.
//
// The relays used to tear the whole thing down when their read side ended: a WebSocket close is
// bidirectional and the wire had no way to say "my write side is finished". So `nc -N`, an HTTP
// client sending Connection: close, and any io.Copy pipeline got an EMPTY reply with a NIL
// ERROR - the answer truncated to nothing and reported as a clean EOF, which is the worst way
// for it to fail. Measured against a real echo before the fix: direct "ECHO:hello", tunnelled "".
//
// awake_test.go covers the same shape through the LOCAL proxy, which always handled it. This is
// the connect path, which did not.
func TestAHalfCloseSurvivesTheConnectTunnel(t *testing.T) {
	// A workload that cannot answer until it has seen the whole request - which is exactly what
	// a half-close is for. One that replied per chunk would hide the bug.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()

		req, _ := io.ReadAll(c) // returns only once the peer's write side is closed
		_, _ = c.Write(append([]byte("ECHO:"), req...))
	}()

	upstream, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// The websocket between the two halves, and the local socket the application holds.
	wsServer, wsClient := tcpPair(t)
	local, app := tcpPair(t)

	go relay(&wsConn{conn: wsServer, br: bufio.NewReader(wsServer)}, upstream)
	go relayClient(&wsConn{conn: wsClient, br: bufio.NewReader(wsClient), mask: true}, local, true)

	if _, err := app.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}

	// The half-close: everything the application meant to send has been sent.
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))

	got, err := io.ReadAll(app)
	if err != nil {
		t.Fatalf("reading the reply after a half-close: %v", err)
	}

	if string(got) != "ECHO:hello" {
		t.Errorf("through the tunnel got %q, want %q - the reply was cut off by the half-close "+
			"rather than delivered", string(got), "ECHO:hello")
	}
}

// The capability is asked for, not assumed. Against a deployment that does not advertise it the
// client must keep the old behaviour: an older sbx writes the empty frame upstream as a no-op
// and never delivers the EOF, so signalling would turn a truncated reply into a wait.
func TestTheHalfCloseSignalIsNotSentToADeploymentThatCannotUseIt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()

		_, _ = io.Copy(io.Discard, c)
	}()

	upstream, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	wsServer, wsClient := tcpPair(t)
	local, app := tcpPair(t)

	go relay(&wsConn{conn: wsServer, br: bufio.NewReader(wsServer)}, upstream)

	// halfClose false: the deployment did not advertise it.
	go relayClient(&wsConn{conn: wsClient, br: bufio.NewReader(wsClient), mask: true}, local, false)

	if _, err := app.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}

	_ = app.CloseWrite()
	_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))

	// The old behaviour: the tunnel is torn down, so the read ends rather than hanging.
	if _, err := io.ReadAll(app); err != nil {
		t.Fatalf("without the capability the tunnel should close, not hang: %v", err)
	}
}
