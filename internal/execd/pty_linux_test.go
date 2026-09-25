//go:build linux

package execd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/wsclient"
)

// ptyClient drives one PTY WebSocket the way upstream's clients do, collecting output and JSON
// frames as they come.
type ptyClient struct {
	t      *testing.T
	c      *wsclient.Conn
	out    strings.Builder // 0x01 and 0x03 payloads, in arrival order
	stderr strings.Builder
	frames []ptyServerFrame
	// replays records the offset of every 0x03 frame.
	replays []int64
	closed  *wsclient.CloseError
}

func (s *testServer) createPTY(body any) string {
	s.t.Helper()

	status, _, data := s.do("POST", "/pty", body)
	if status != http.StatusCreated {
		s.t.Fatalf("POST /pty: %d %s", status, data)
	}

	var resp struct {
		SessionID string `json:"session_id"`
	}

	if err := json.Unmarshal(data, &resp); err != nil || resp.SessionID == "" {
		s.t.Fatalf("POST /pty returned %s", data)
	}

	return resp.SessionID
}

func (s *testServer) dialPTY(id, query string) (*ptyClient, error) {
	s.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	u := "ws" + strings.TrimPrefix(s.ts.URL, "http") + "/pty/" + id + "/ws"
	if query != "" {
		u += "?" + query
	}

	c, err := wsclient.Dial(ctx, u, wsclient.Options{})
	if err != nil {
		return nil, err
	}

	s.t.Cleanup(func() { _ = c.Close() })

	return &ptyClient{t: s.t, c: c}, nil
}

func (s *testServer) mustDialPTY(id, query string) *ptyClient {
	s.t.Helper()

	p, err := s.dialPTY(id, query)
	if err != nil {
		s.t.Fatal(err)
	}

	return p
}

// read takes one message and files it. It reports false once the connection is gone.
func (p *ptyClient) read(within time.Duration) bool {
	p.t.Helper()

	_ = p.c.SetReadDeadline(time.Now().Add(within))

	typ, data, err := p.c.ReadMessage()
	if err != nil {
		var ce *wsclient.CloseError
		if errors.As(err, &ce) {
			p.closed = ce
		}

		return false
	}

	if typ == wsclient.TextMessage {
		var f ptyServerFrame
		if err := json.Unmarshal(data, &f); err != nil {
			p.t.Fatalf("text frame is not JSON: %q", data)
		}

		p.frames = append(p.frames, f)

		return true
	}

	if len(data) == 0 {
		p.t.Fatal("empty binary frame")
	}

	switch data[0] {
	case ptyBinStdout:
		p.out.Write(data[1:])
	case ptyBinStderr:
		p.stderr.Write(data[1:])
	case ptyBinReplay:
		if len(data) < 9 {
			p.t.Fatalf("replay frame of %d bytes has no offset", len(data))
		}

		p.replays = append(p.replays, int64(binary.BigEndian.Uint64(data[1:9])))
		p.out.Write(data[9:])
	default:
		p.t.Fatalf("unknown binary frame type %#x", data[0])
	}

	return true
}

// until reads until cond holds, failing after 10s.
func (p *ptyClient) until(what string, cond func() bool) {
	p.t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for !cond() {
		if time.Now().After(deadline) || !p.read(time.Until(deadline)) {
			if cond() {
				return
			}

			p.t.Fatalf("never saw %s; output %q stderr %q frames %+v close %v", what, p.out.String(), p.stderr.String(), p.frames, p.closed)
		}
	}
}

func (p *ptyClient) frame(typ string) *ptyServerFrame {
	for i := range p.frames {
		if p.frames[i].Type == typ {
			return &p.frames[i]
		}
	}

	return nil
}

func (p *ptyClient) stdin(s string) {
	p.t.Helper()

	if err := p.c.WriteMessage(wsclient.BinaryMessage, append([]byte{ptyBinStdin}, s...)); err != nil {
		p.t.Fatal(err)
	}
}

func (p *ptyClient) sendJSON(v any) {
	p.t.Helper()

	b, _ := json.Marshal(v)
	if err := p.c.WriteText(b); err != nil {
		p.t.Fatal(err)
	}
}

func TestPTYHolderShellResizeAndExit(t *testing.T) {
	s := newTestServer(t, Options{})
	id := s.createPTY(map[string]any{"cwd": t.TempDir()})

	// Nothing runs until the first holder connects.
	_, _, body := s.do("GET", "/pty/"+id, nil)
	if !strings.Contains(string(body), `"running":false`) {
		t.Fatalf("status before connect: %s", body)
	}

	h := s.mustDialPTY(id, "")
	h.until("connected", func() bool { return h.frame("connected") != nil })

	if f := h.frame("connected"); f.Mode != "pty" || f.Role != "holder" || f.SessionID != id {
		t.Fatalf("connected frame %+v", f)
	}

	h.stdin("echo hi-$((40+2)); tty\n")
	h.until("command output", func() bool { return strings.Contains(h.out.String(), "hi-42") })
	h.until("a real terminal", func() bool { return strings.Contains(h.out.String(), "/dev/pts/") })

	h.sendJSON(ptyClientFrame{Type: "resize", Cols: 123, Rows: 45})
	h.stdin("stty size\n")
	h.until("the new size", func() bool { return strings.Contains(h.out.String(), "45 123") })

	h.sendJSON(ptyClientFrame{Type: "ping"})
	h.until("pong", func() bool { return h.frame("pong") != nil })

	h.sendJSON(ptyClientFrame{Type: "bogus"})
	h.until("invalid frame error", func() bool { f := h.frame("error"); return f != nil && f.Code == wsErrInvalidFrame })

	_, _, body = s.do("GET", "/pty/"+id, nil)

	var st struct {
		Running bool  `json:"running"`
		Offset  int64 `json:"output_offset"`
	}

	if err := json.Unmarshal(body, &st); err != nil || !st.Running || st.Offset == 0 {
		t.Fatalf("status while running: %s", body)
	}

	// JSON stdin is the debugging fallback upstream accepts.
	h.sendJSON(ptyClientFrame{Type: "stdin", Data: "exit 7\n"})
	h.until("exit frame", func() bool { return h.frame("exit") != nil })

	if f := h.frame("exit"); f.ExitCode == nil || *f.ExitCode != 7 {
		t.Fatalf("exit frame %+v", f)
	}

	h.until("close", func() bool { return h.closed != nil })

	if h.closed.Code != wsclient.CloseNormal {
		t.Fatalf("closed with %d, want 1000", h.closed.Code)
	}
}

func TestPTYOneHolderViewersAndTakeover(t *testing.T) {
	s := newTestServer(t, Options{})
	id := s.createPTY(nil)

	// A viewer cannot start the shell.
	_, err := s.dialPTY(id, "mode=viewer")

	var he *wsclient.HandshakeError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || !strings.Contains(he.Body, wsErrViewerNotRunning) {
		t.Fatalf("viewer before start: %v", err)
	}

	h1 := s.mustDialPTY(id, "")
	h1.until("connected", func() bool { return h1.frame("connected") != nil })
	h1.stdin("echo first-$((1+1))\n")
	h1.until("output", func() bool { return strings.Contains(h1.out.String(), "first-2") })

	// A second read/write client is refused over HTTP, before any upgrade.
	_, err = s.dialPTY(id, "")
	if !errors.As(err, &he) || he.Status != http.StatusConflict || !strings.Contains(he.Body, wsErrAlreadyConnected) {
		t.Fatalf("second holder: %v", err)
	}

	// A viewer replays from the start, then follows live output, and may not type.
	v := s.mustDialPTY(id, "mode=viewer&since=0")
	v.until("replayed scrollback", func() bool { return strings.Contains(v.out.String(), "first-2") })
	v.until("connected", func() bool { return v.frame("connected") != nil })

	if f := v.frame("connected"); f.Role != "viewer" || len(v.replays) == 0 || v.replays[0] != 0 {
		t.Fatalf("viewer connected %+v replays %v", f, v.replays)
	}

	v.stdin("echo nope\n")
	v.until("read-only refusal", func() bool { f := v.frame("error"); return f != nil && f.Code == wsErrReadOnly })

	h1.stdin("echo live-$((2+2))\n")
	v.until("live output as replay frames", func() bool { return strings.Contains(v.out.String(), "live-4") })

	// Takeover: the first holder is closed with 4001, the shell keeps running, and the new
	// holder gets the scrollback it asked for.
	h2 := s.mustDialPTY(id, "takeover=1&since=0")
	h2.until("connected", func() bool { return h2.frame("connected") != nil })

	if !strings.Contains(h2.out.String(), "live-4") {
		t.Fatalf("takeover with since=0 did not replay: %q", h2.out.String())
	}

	h1.until("eviction", func() bool { return h1.closed != nil })

	if h1.closed.Code != wsCloseTakenOver || h1.closed.Text != wsErrTakenOver {
		t.Fatalf("evicted holder closed with %+v", h1.closed)
	}

	h2.stdin("echo after-$((3+3))\n")
	h2.until("output after takeover", func() bool { return strings.Contains(h2.out.String(), "after-6") })

	// Deleting the session ends everyone attached and forgets it.
	if status, _, body := s.do("DELETE", "/pty/"+id, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %s", status, body)
	}

	h2.until("holder ended", func() bool { return h2.closed != nil || !h2.read(time.Second) })
	v.until("viewer exit", func() bool { return v.frame("exit") != nil })

	status, _, body := s.do("GET", "/pty/"+id, nil)
	wantError(t, status, body, http.StatusNotFound, codeContextNotFound)
}

func TestPTYPipeModeSeparatesStderrAndSignals(t *testing.T) {
	s := newTestServer(t, Options{})
	id := s.createPTY(map[string]string{"command": "echo to-out; echo to-err >&2; exec sleep 30"})

	h := s.mustDialPTY(id, "pty=0")
	h.until("both streams", func() bool {
		return strings.Contains(h.out.String(), "to-out") && strings.Contains(h.stderr.String(), "to-err")
	})

	if f := h.frame("connected"); f == nil || f.Mode != "pipe" {
		t.Fatalf("connected %+v", f)
	}

	if strings.Contains(h.out.String(), "to-err") {
		t.Fatal("stderr leaked into stdout frames in pipe mode")
	}

	h.sendJSON(ptyClientFrame{Type: "signal", Signal: "SIGTERM"})
	h.until("exit", func() bool { return h.frame("exit") != nil })

	if f := h.frame("exit"); *f.ExitCode != 128+15 {
		t.Fatalf("exit code %d, want 143 for SIGTERM", *f.ExitCode)
	}
}

func TestPTYReconnectReplaysFromOffset(t *testing.T) {
	s := newTestServer(t, Options{})
	id := s.createPTY(map[string]string{"command": "echo one; read x; echo two-$x; read y"})

	h := s.mustDialPTY(id, "")
	h.until("one", func() bool { return strings.Contains(h.out.String(), "one") })
	h.c.Close()

	_, _, body := s.do("GET", "/pty/"+id, nil)

	var st struct {
		Offset int64 `json:"output_offset"`
	}
	_ = json.Unmarshal(body, &st)

	// The holder slot frees once the old connection is torn down.
	var h2 *ptyClient

	eventually(t, 5*time.Second, "the holder slot to free", func() bool {
		var err error
		h2, err = s.dialPTY(id, "since="+strconv.FormatInt(st.Offset, 10))

		return err == nil
	})

	h2.until("connected", func() bool { return h2.frame("connected") != nil })

	if strings.Contains(h2.out.String(), "one") {
		t.Fatalf("since=%d replayed output from before it: %q", st.Offset, h2.out.String())
	}

	h2.stdin("abc\n")
	h2.until("output after reconnect", func() bool { return strings.Contains(h2.out.String(), "two-abc") })
}

func TestPTYStartFailureAndBadRequests(t *testing.T) {
	s := newTestServer(t, Options{})

	status, _, body := s.do("POST", "/pty", "{nope")
	wantError(t, status, body, http.StatusBadRequest, codeInvalidRequest)

	status, _, body = s.do("GET", "/pty/nope/ws", nil)
	wantError(t, status, body, http.StatusNotFound, codeContextNotFound)

	status, _, body = s.do("DELETE", "/pty/nope", nil)
	wantError(t, status, body, http.StatusNotFound, codeContextNotFound)

	// A session whose working directory vanished fails to start, and says so on the socket.
	dir := t.TempDir() + "/gone"
	id := s.createPTY(map[string]string{"cwd": dir})

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	h := s.mustDialPTY(id, "")
	h.until("start failure", func() bool { f := h.frame("error"); return f != nil && f.Code == wsErrStartFailed })
}
