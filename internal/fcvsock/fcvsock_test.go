package fcvsock

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFirecracker listens where a Firecracker vsock device would and answers the hybrid-vsock
// handshake with whatever reply says. It is the host half of the device only: the tests are
// about the dialer, and the protocol on this side of the socket is all the dialer ever sees.
type fakeFirecracker struct {
	path  string
	ln    net.Listener
	lines chan string
}

// newFake serves every connection with serve, after recording the CONNECT line it read.
func newFake(t *testing.T, serve func(c net.Conn, line string)) *fakeFirecracker {
	t.Helper()

	// A short path: sun_path is 104 bytes on darwin and t.TempDir() under /var/folders can
	// come close enough to it that a longer test name would fail for a reason unrelated to vsock.
	dir, err := os.MkdirTemp("", "fcv")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "v.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	f := &fakeFirecracker{path: path, ln: ln, lines: make(chan string, 16)}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go func() {
				defer c.Close()

				// Byte at a time, as Firecracker does: nothing after the newline may be
				// consumed here, or a test of the dialer's own buffering proves nothing.
				var sb strings.Builder

				b := make([]byte, 1)

				for {
					if _, err := c.Read(b); err != nil {
						return
					}

					if b[0] == '\n' {
						break
					}

					sb.WriteByte(b[0])
				}

				f.lines <- sb.String()
				serve(c, sb.String())
			}()
		}
	}()

	return f
}

func echoAfter(reply string) func(net.Conn, string) {
	return func(c net.Conn, _ string) {
		if _, err := io.WriteString(c, reply); err != nil {
			return
		}

		_, _ = io.Copy(c, c)
	}
}

func TestDialHandshakeThenStream(t *testing.T) {
	f := newFake(t, echoAfter("OK 1073741824\n"))

	c, err := Dial(context.Background(), f.path, 44772)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if got := <-f.lines; got != "CONNECT 44772" {
		t.Fatalf("handshake line %q, want %q", got, "CONNECT 44772")
	}

	if hp := c.(*Conn).HostPort; hp != 1073741824 {
		t.Fatalf("HostPort %d, want the number from the OK line", hp)
	}

	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}

	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo through the dialled conn: %q %v", line, err)
	}
}

// A guest that speaks first - a server banner, or a response racing the OK - must not lose the
// bytes that arrived in the same read as the handshake reply.
func TestDialKeepsBytesThatFollowOK(t *testing.T) {
	f := newFake(t, func(c net.Conn, _ string) {
		_, _ = io.WriteString(c, "OK 7\nbanner\n")
		time.Sleep(200 * time.Millisecond)
	})

	c, err := Dial(context.Background(), f.path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "banner\n" {
		t.Fatalf("first bytes after the handshake: %q %v, want the guest's banner", line, err)
	}
}

// The OK line split across writes is how a slow or loaded VMM delivers it; the dialer reads
// until the newline rather than trusting one read to hold it.
func TestDialPartialReply(t *testing.T) {
	f := newFake(t, func(c net.Conn, _ string) {
		for _, part := range []string{"O", "K 12", "34", "\n"} {
			_, _ = io.WriteString(c, part)
			time.Sleep(20 * time.Millisecond)
		}

		_, _ = io.Copy(c, c)
	})

	c, err := Dial(context.Background(), f.path, 2)
	if err != nil {
		t.Fatalf("dial with a reply in four pieces: %v", err)
	}
	defer c.Close()

	if hp := c.(*Conn).HostPort; hp != 1234 {
		t.Fatalf("HostPort %d, want 1234", hp)
	}
}

// Firecracker's refusal is a hang-up with no reply: nothing listens on that guest port.
func TestDialRefused(t *testing.T) {
	f := newFake(t, func(net.Conn, string) {})

	_, err := Dial(context.Background(), f.path, 44772)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v, want ErrRefused", err)
	}

	if !strings.Contains(err.Error(), "44772") || !strings.Contains(err.Error(), "--vsock-port") {
		t.Fatalf("the refusal should name the port and what to check: %v", err)
	}
}

// Anything but OK is refused too, and the reply is quoted so it can be diagnosed.
func TestDialGarbageReply(t *testing.T) {
	f := newFake(t, echoAfter("NOPE\n"))

	_, err := Dial(context.Background(), f.path, 3)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "NOPE") {
		t.Fatalf("got %v, want ErrRefused quoting the reply", err)
	}
}

// A reply that never ends must not be read forever into memory.
func TestDialOverlongReply(t *testing.T) {
	f := newFake(t, func(c net.Conn, _ string) {
		_, _ = io.WriteString(c, "OK "+strings.Repeat("9", 4096))
		time.Sleep(time.Second)
	})

	_, err := Dial(context.Background(), f.path, 3)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v, want ErrRefused for an unterminated reply", err)
	}
}

// A VMM that accepts and then says nothing - a paused VM, a wedged process - is bounded by the
// dialer's own timeout when the caller set no deadline.
func TestDialTimeout(t *testing.T) {
	f := newFake(t, func(net.Conn, string) { time.Sleep(5 * time.Second) })

	start := time.Now()

	_, err := Dialer{UDSPath: f.path, Port: 5, Timeout: 150 * time.Millisecond}.DialContext(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want ErrTimeout", err)
	}

	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("timed out after %v, want about 150ms", el)
	}
}

// The caller's context wins over the timeout, in both directions: cancelled mid-handshake it
// returns at once with the context's error.
func TestDialContextCancelled(t *testing.T) {
	f := newFake(t, func(net.Conn, string) { time.Sleep(5 * time.Second) })

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()

	_, err := Dialer{UDSPath: f.path, Port: 5, Timeout: 10 * time.Second}.DialContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}

	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("returned after %v, want promptly after the cancel", el)
	}
}

// The deadline used for the handshake is cleared afterwards: the conn is a long-lived stream,
// and a deadline left on it would cut a quiet session at the dial timeout.
func TestDialClearsHandshakeDeadline(t *testing.T) {
	f := newFake(t, func(c net.Conn, _ string) {
		_, _ = io.WriteString(c, "OK 1\n")
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(c, "late\n")
		time.Sleep(200 * time.Millisecond)
	})

	c, err := Dialer{UDSPath: f.path, Port: 1, Timeout: 100 * time.Millisecond}.DialContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "late\n" {
		t.Fatalf("read after the dial timeout elapsed: %q %v", line, err)
	}
}

func TestDialNoSocket(t *testing.T) {
	_, err := Dial(context.Background(), filepath.Join(t.TempDir(), "absent.sock"), 1)
	if !errors.Is(err, ErrNoVM) {
		t.Fatalf("got %v, want ErrNoVM", err)
	}
}

// Half-close passes through, so the wake proxy's CloseWrite still tells the guest "that is all".
func TestConnCloseWrite(t *testing.T) {
	got := make(chan string, 1)

	f := newFake(t, func(c net.Conn, _ string) {
		_, _ = io.WriteString(c, "OK 1\n")
		b, _ := io.ReadAll(c)
		got <- string(b)
	})

	c, err := Dial(context.Background(), f.path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _ = io.WriteString(c, "all of it")

	cw, ok := c.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("Conn does not expose CloseWrite")
	}

	if err := cw.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	select {
	case s := <-got:
		if s != "all of it" {
			t.Fatalf("guest read %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guest never saw EOF after CloseWrite")
	}
}

func TestDialerValidates(t *testing.T) {
	if _, err := (Dialer{Port: 1}).DialContext(context.Background()); err == nil {
		t.Fatal("an empty UDSPath dialled")
	}
}
