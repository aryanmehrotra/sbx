//go:build unix

package execd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// socketPair returns a connected AF_UNIX stream pair: the server end as a raw non-blocking fd,
// exactly as accept4(SOCK_NONBLOCK) hands execd a vsock conn, and the client end as a net.Conn.
func socketPair(t *testing.T) (serverFD int, client net.Conn) {
	t.Helper()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}

	for _, fd := range fds {
		syscall.CloseOnExec(fd)

		if err := syscall.SetNonblock(fd, true); err != nil {
			t.Fatalf("nonblock: %v", err)
		}
	}

	cf := os.NewFile(uintptr(fds[1]), "client")
	defer func() { _ = cf.Close() }()

	client, err = net.FileConn(cf)
	if err != nil {
		t.Fatalf("FileConn: %v", err)
	}

	return fds[0], client
}

// serveOneConn serves h over conn alone, and returns a client whose every request goes over the
// one client conn - so the second request reuses the keep-alive connection, which is where the
// fault lived.
func serveOneConn(t *testing.T, conn net.Conn, client net.Conn, h http.Handler) *http.Client {
	t.Helper()

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	l := &oneConnListener{conn: conn, done: make(chan struct{})}

	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	var once sync.Once

	tr := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			var c net.Conn

			once.Do(func() { c = client })

			if c == nil {
				return nil, errors.New("test dialled twice: the keep-alive connection was not reused")
			}

			return c, nil
		},
		MaxIdleConnsPerHost: 1,
	}
	t.Cleanup(tr.CloseIdleConnections)

	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

type oneConnListener struct {
	mu   sync.Mutex
	conn net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()

	if c != nil {
		return c, nil
	}

	<-l.done

	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return vsockAddr{Port: 1} }

// TestFileConnKeepsRequestContextsLive is the microVM code-interpreter failure (CI run 36227693829,
// seven TestCodeInterpreter_* timing out on "Jupyter did not answer within 3m0s" while the guest
// console showed Jupyter up): over the vsock conn, every request after the first on a keep-alive
// connection arrived with r.Context() already cancelled, so /ping?ready=code's Jupyter probe
// failed instantly with "context canceled", 721 times in a row. Over TCP the same test passes.
func TestFileConnKeepsRequestContextsLive(t *testing.T) {
	fd, client := socketPair(t)
	conn := &fileConn{File: os.NewFile(uintptr(fd), "vsock-conn"), local: vsockAddr{Port: 1}, remote: vsockAddr{CID: 2, Port: 9}}

	var (
		mu   sync.Mutex
		errs []error
	)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Long enough for a cancellation already in flight to have landed; a live request's
		// context is still nil-Err after it.
		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		errs = append(errs, r.Context().Err())
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	})

	c := serveOneConn(t, conn, client, h)

	for i := range 3 {
		resp, err := c.Get("http://execd/ping")
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()

	if len(errs) != 3 {
		t.Fatalf("handler ran %d times, want 3", len(errs))
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d over one keep-alive conn: r.Context().Err() = %v while the client was still "+
				"waiting; want nil", i+1, err)
		}
	}
}

// TestFileConnErrorsAreNetErrors pins the contract the fix rests on: a read that hits its
// deadline is a net.Error with Timeout() true - what net/http asserts before it decides a
// read error is the expected abort and not a dead client - and still wraps
// os.ErrDeadlineExceeded; EOF stays io.EOF; a closed conn reads net.ErrClosed.
func TestFileConnErrorsAreNetErrors(t *testing.T) {
	fd, client := socketPair(t)
	conn := &fileConn{File: os.NewFile(uintptr(fd), "vsock-conn"), local: vsockAddr{Port: 1}, remote: vsockAddr{CID: 2, Port: 9}}

	if err := conn.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v (the fd is not pollable)", err)
	}

	_, err := conn.Read(make([]byte, 1))

	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read past deadline: %T %v; want a net.Error with Timeout()", err, err)
	}

	if _, ok := err.(net.Error); !ok {
		t.Fatalf("read past deadline: %T is not itself a net.Error; net/http type-asserts, it does not unwrap", err)
	}

	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read past deadline: %v does not wrap os.ErrDeadlineExceeded", err)
	}

	_ = conn.SetReadDeadline(time.Time{})
	_ = client.Close()

	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read after peer close: %v; want io.EOF itself", err)
	}

	_ = conn.Close()

	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after Close: %v; want net.ErrClosed", err)
	}
}
