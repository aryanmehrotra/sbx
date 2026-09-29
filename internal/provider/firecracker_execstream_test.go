//go:build unix

package provider

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/wsserver"
)

// `sbx exec` on a microVM streams stdout and stderr apart and returns the command's own status
// as a value, not an error - the CLI turns it into sbx's exit code.
func TestFirecrackerExecStreamReturnsTheStatus(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "x6", redis)

	var started atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("POST /pty", func(w http.ResponseWriter, _ *http.Request) {
		started.Store(true)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"session_id":"p1"}`)
	})
	mux.HandleFunc("DELETE /pty/p1", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /pty/p1/ws", func(w http.ResponseWriter, req *http.Request) {
		c, err := wsserver.Upgrade(w, req, wsserver.Options{})
		if err != nil {
			return
		}
		defer c.Close()

		_ = c.WriteMessage(wsserver.BinaryMessage, append([]byte{0x01}, "a\x00b\r\n"...))
		_ = c.WriteMessage(wsserver.BinaryMessage, append([]byte{0x02}, "warn"...))
		_ = c.WriteMessage(wsserver.TextMessage, []byte(`{"type":"exit","exit_code":7}`))
	})

	serveGuest(t, r, mux)

	var out, errw bytes.Buffer

	code, err := r.p.ExecStream(r.ctx, ref, []string{"sh", "-c", "exit 7"}, nil, &out, &errw)
	if err != nil || code != 7 {
		t.Fatalf("ExecStream = (%d, %v), want (7, nil)", code, err)
	}

	// Byte for byte: a NUL and a CRLF survive, which the line-cut SSE stream would not.
	if out.String() != "a\x00b\r\n" || errw.String() != "warn" {
		t.Errorf("stdout %q stderr %q", out.String(), errw.String())
	}

	// Stdin with data in it is refused before anything runs in the VM: execd cannot signal
	// EOF, so the alternative is a command that never exits, or input silently dropped.
	started.Store(false)

	_, err = r.p.ExecStream(r.ctx, ref, []string{"cat"}, strings.NewReader("hi\n"), &out, &errw)
	if !errors.Is(err, errStdinUnsupported) {
		t.Fatalf("piped stdin: err = %v, want errStdinUnsupported", err)
	}

	if started.Load() {
		t.Error("a session was started in the VM before stdin was refused")
	}

	// Empty stdin (a closed pipe, </dev/null) is nothing to lose, so it runs.
	if code, err := r.p.ExecStream(r.ctx, ref, []string{"true"}, strings.NewReader(""), &out, &errw); err != nil || code != 7 {
		t.Fatalf("empty stdin: ExecStream = (%d, %v), want it to run", code, err)
	}
}

// A pipe that stays open and silent is treated as empty after the grace period rather than
// hanging the command, and data that turns up later is reported, not lost without a word.
func TestRefuseStdinDataWarnsAboutLateInput(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	var errw lockedBuffer

	if err := refuseStdinData(pr, 20*time.Millisecond, &errw); err != nil {
		t.Fatalf("a silent open pipe was refused: %v", err)
	}

	// One byte: an io.Pipe write blocks until it is all read, and the check reads one.
	_, _ = pw.Write([]byte("l"))

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(errw.String(), "was not passed") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if !strings.Contains(errw.String(), "was not passed") {
		t.Errorf("late stdin was dropped silently; stderr = %q", errw.String())
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
