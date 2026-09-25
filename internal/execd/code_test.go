//go:build unix

package execd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/jupyter"
	"github.com/aryanmehrotra/sbx/internal/wsclient/wstest"
)

// miniJupyter is the smallest Jupyter Server the engine will talk to: kernelspecs, sessions,
// interrupt and the kernel channel. The engine's own tests cover the protocol in depth; this
// exists to check execd's routing and status codes against a real engine rather than a mock.
//
// Code is interpreted by line: "print:x" streams x to stdout, "sleep" blocks until interrupted.
type miniJupyter struct {
	srv        *httptest.Server
	mu         sync.Mutex
	sessions   map[string]string // session id -> kernel id
	kernels    map[string]chan struct{}
	n          int
	interrupts atomic.Int32
	specHits   atomic.Int32
	downUntil  atomic.Int64 // unix nanos; kernelspecs answers 503 before it, like a server still starting
	running    chan string  // kernel id, sent when a "sleep" starts
}

func newMiniJupyter(t *testing.T) *miniJupyter {
	t.Helper()

	f := &miniJupyter{sessions: map[string]string{}, kernels: map[string]chan struct{}{}, running: make(chan string, 4)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *miniJupyter) engine() *jupyter.Engine {
	return jupyter.New(jupyter.Config{BaseURL: f.srv.URL, StartupWait: time.Second, PingInterval: time.Hour})
}

func (f *miniJupyter) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}

	switch {
	case r.Method == "GET" && p == "/api/kernelspecs":
		f.specHits.Add(1)

		if time.Now().UnixNano() < f.downUntil.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}

		reply(200, map[string]any{"default": "python3", "kernelspecs": map[string]any{
			"python3": map[string]any{"name": "python3", "spec": map[string]any{"language": "python"}},
		}})
	case r.Method == "POST" && p == "/api/sessions":
		f.mu.Lock()
		f.n++
		sid, kid := fmt.Sprintf("s%d", f.n), fmt.Sprintf("k%d", f.n)
		f.sessions[sid], f.kernels[kid] = kid, make(chan struct{}, 1)
		f.mu.Unlock()
		reply(201, map[string]any{"id": sid, "kernel": map[string]any{"id": kid, "name": "python3"}})
	case r.Method == "DELETE" && strings.HasPrefix(p, "/api/sessions/"):
		f.mu.Lock()
		delete(f.sessions, strings.TrimPrefix(p, "/api/sessions/"))
		f.mu.Unlock()
		w.WriteHeader(204)
	case r.Method == "POST" && strings.HasSuffix(p, "/interrupt"):
		kid := strings.TrimSuffix(strings.TrimPrefix(p, "/api/kernels/"), "/interrupt")
		f.mu.Lock()
		ch := f.kernels[kid]
		f.mu.Unlock()
		f.interrupts.Add(1)

		if ch != nil {
			select {
			case ch <- struct{}{}:
			default:
			}
		}

		w.WriteHeader(204)
	case strings.HasSuffix(p, "/channels"):
		kid := strings.TrimSuffix(strings.TrimPrefix(p, "/api/kernels/"), "/channels")
		f.mu.Lock()
		intr := f.kernels[kid]
		f.mu.Unlock()

		c, err := wstest.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()

		f.channel(c, kid, intr)
	default:
		http.NotFound(w, r)
	}
}

func (f *miniJupyter) channel(c *wstest.Conn, kid string, intr chan struct{}) {
	for {
		_, p, err := c.ReadMessage()
		if err != nil {
			return
		}

		var req struct {
			Header struct {
				MsgID   string `json:"msg_id"`
				MsgType string `json:"msg_type"`
			} `json:"header"`
			Content struct{ Code string } `json:"content"`
		}
		_ = json.Unmarshal(p, &req)

		send := func(channel, typ string, content any) {
			b, _ := json.Marshal(map[string]any{
				"header": map[string]any{"msg_id": "srv", "msg_type": typ}, "channel": channel, "content": content,
				"parent_header": map[string]any{"msg_id": req.Header.MsgID}, "metadata": map[string]any{},
			})
			_ = c.WriteText(b)
		}

		if req.Header.MsgType == "kernel_info_request" {
			send("shell", "kernel_info_reply", map[string]any{"status": "ok"})
			continue
		}

		status := "ok"

		send("iopub", "status", map[string]any{"execution_state": "busy"})

		for _, line := range strings.Split(req.Content.Code, "\n") {
			verb, arg, _ := strings.Cut(line, ":")
			switch verb {
			case "print":
				send("iopub", "stream", map[string]any{"name": "stdout", "text": arg + "\n"})
			case "sleep":
				f.running <- kid
				<-intr
				send("iopub", "error", map[string]any{"ename": "KeyboardInterrupt", "evalue": "", "traceback": []string{}})
				status = "error"
			}
		}

		send("iopub", "status", map[string]any{"execution_state": "idle"})
		send("shell", "execute_reply", map[string]any{"status": status, "execution_count": 1})
	}
}

func TestCodeWithoutJupyterAnswers501WithTheReason(t *testing.T) {
	s := newTestServer(t, Options{Jupyter: jupyter.New(jupyter.Config{})})

	cases := []struct {
		method, path string
		body         any
	}{
		{"POST", "/code/context", map[string]string{"language": "python"}},
		{"GET", "/code/contexts?language=python", nil},
		{"DELETE", "/code/contexts?language=python", nil},
		{"GET", "/code/contexts/abc", nil},
		{"DELETE", "/code/contexts/abc", nil},
		{"POST", "/code", map[string]any{"context": map[string]string{"language": "python"}, "code": "1+1"}},
	}

	for _, c := range cases {
		status, _, body := s.do(c.method, c.path, c.body)
		wantError(t, status, body, http.StatusNotImplemented, codeNotSupported)

		if !strings.Contains(string(body), jupyter.EnvHost) {
			t.Errorf("%s %s: the 501 does not say what to set: %s", c.method, c.path, body)
		}
	}
}

// Upstream runs language-less code as a shell command, and that must not depend on Jupyter.
func TestCodeWithoutALanguageRunsAsACommand(t *testing.T) {
	s := newTestServer(t, Options{Jupyter: jupyter.New(jupyter.Config{})})

	exec := s.stream("POST", "/code", map[string]any{"code": "echo from-code"})
	if exec.Text() != "from-code" || !exec.Complete {
		t.Fatalf("stdout %q complete %v", exec.Stdout, exec.Complete)
	}

	status, _, body := s.do("POST", "/code", map[string]any{"context": map[string]string{"language": "cobol"}, "code": "x"})
	wantError(t, status, body, http.StatusBadRequest, codeInvalidRequest)

	status, _, body = s.do("POST", "/code", map[string]any{"code": ""})
	wantError(t, status, body, http.StatusBadRequest, codeInvalidRequest)

	status, _, body = s.do("POST", "/code", "{not json")
	wantError(t, status, body, http.StatusBadRequest, codeInvalidRequest)
}

func TestCodeContextLifecycle(t *testing.T) {
	f := newMiniJupyter(t)
	s := newTestServer(t, Options{Jupyter: f.engine()})

	status, _, body := s.do("POST", "/code/context", map[string]string{"language": "Python", "cwd": t.TempDir()})
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}

	var ctx codeContextResponse
	if err := json.Unmarshal(body, &ctx); err != nil || ctx.ID == "" || ctx.Language != "python" || ctx.Cwd == "" {
		t.Fatalf("create returned %s (%v)", body, err)
	}

	status, _, body = s.do("POST", "/code/context", map[string]string{"language": "cobol"})
	wantError(t, status, body, http.StatusBadRequest, codeInvalidRequest)

	status, _, body = s.do("POST", "/code/context", "{")
	wantError(t, status, body, http.StatusBadRequest, codeInvalidRequest)

	status, _, body = s.do("GET", "/code/contexts/"+ctx.ID, nil)
	if status != http.StatusOK || !strings.Contains(string(body), ctx.ID) {
		t.Fatalf("get: %d %s", status, body)
	}

	status, _, body = s.do("GET", "/code/contexts/nope", nil)
	wantError(t, status, body, http.StatusNotFound, codeContextNotFound)

	var list []codeContextResponse

	status, _, body = s.do("GET", "/code/contexts?language=python", nil)
	if err := json.Unmarshal(body, &list); status != http.StatusOK || err != nil || len(list) != 1 {
		t.Fatalf("list python: %d %s", status, body)
	}

	status, _, body = s.do("GET", "/code/contexts?language=go", nil)
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("an empty list must be [] not null: %d %s", status, body)
	}

	status, _, body = s.do("DELETE", "/code/contexts/nope", nil)
	wantError(t, status, body, http.StatusNotFound, codeContextNotFound)

	status, _, body = s.do("DELETE", "/code/contexts", nil)
	wantError(t, status, body, http.StatusBadRequest, codeMissingQuery)

	if status, _, body = s.do("DELETE", "/code/contexts/"+ctx.ID, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %s", status, body)
	}

	status, _, body = s.do("GET", "/code/contexts/"+ctx.ID, nil)
	wantError(t, status, body, http.StatusNotFound, codeContextNotFound)

	// Two more, then both gone by language.
	for range 2 {
		if status, _, body = s.do("POST", "/code/context", map[string]string{"language": "python"}); status != http.StatusOK {
			t.Fatalf("create: %d %s", status, body)
		}
	}

	if status, _, body = s.do("DELETE", "/code/contexts?language=python", nil); status != http.StatusOK {
		t.Fatalf("delete by language: %d %s", status, body)
	}

	if _, _, body = s.do("GET", "/code/contexts", nil); strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("contexts left after delete by language: %s", body)
	}
}

func TestRunCodeStreamsTheKernel(t *testing.T) {
	f := newMiniJupyter(t)
	s := newTestServer(t, Options{Jupyter: f.engine()})

	exec := s.stream("POST", "/code", map[string]any{"context": map[string]string{"language": "python"}, "code": "print:hello"})
	if strings.TrimSpace(exec.Text()) != "hello" || !exec.Complete || exec.ID == "" {
		t.Fatalf("stdout %q complete %v id %q types %v", exec.Stdout, exec.Complete, exec.ID, exec.Types)
	}

	// The implicit context is kept and can be named by id alone.
	exec2 := s.stream("POST", "/code", map[string]any{"context": map[string]string{"id": exec.ID}, "code": "print:again"})
	if strings.TrimSpace(exec2.Text()) != "again" || exec2.ID != exec.ID {
		t.Fatalf("by id: stdout %q id %q want %q", exec2.Stdout, exec2.ID, exec.ID)
	}

	status, _, body := s.do("POST", "/code", map[string]any{"context": map[string]string{"id": "nope", "language": "python"}, "code": "x"})
	wantError(t, status, body, http.StatusNotFound, codeContextNotFound)
}

func TestRunCodeBusyAndInterrupt(t *testing.T) {
	f := newMiniJupyter(t)
	s := newTestServer(t, Options{Jupyter: f.engine()})

	_, _, body := s.do("POST", "/code/context", map[string]string{"language": "python"})

	var ctx codeContextResponse
	_ = json.Unmarshal(body, &ctx)

	done := make(chan *sdkExecution, 1)

	go func() {
		done <- s.stream("POST", "/code", map[string]any{"context": map[string]string{"id": ctx.ID, "language": "python"}, "code": "sleep"})
	}()

	select {
	case <-f.running:
	case <-time.After(5 * time.Second):
		t.Fatal("the cell never started")
	}

	// A second execution on a busy context is refused with a 500, as upstream refuses it.
	status, _, body := s.do("POST", "/code", map[string]any{"context": map[string]string{"id": ctx.ID, "language": "python"}, "code": "print:x"})
	wantError(t, status, body, http.StatusInternalServerError, codeRuntimeError)

	if status, _, body = s.do("DELETE", "/code?id="+ctx.ID, nil); status != http.StatusOK {
		t.Fatalf("interrupt: %d %s", status, body)
	}

	select {
	case exec := <-done:
		if exec.ErrName != "KeyboardInterrupt" {
			t.Fatalf("interrupted cell ended with %q, want KeyboardInterrupt; types %v", exec.ErrName, exec.Types)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the interrupted cell never ended")
	}

	if f.interrupts.Load() != 1 {
		t.Fatalf("interrupts reaching Jupyter: %d", f.interrupts.Load())
	}

	status, _, body = s.do("DELETE", "/code", nil)
	wantError(t, status, body, http.StatusBadRequest, codeMissingQuery)

	// An id that is not a kernel's falls through to commands and sessions.
	status, _, body = s.do("DELETE", "/code?id=nope", nil)
	wantError(t, status, body, http.StatusNotFound, codeContextNotFound)
}

// A positive probe is remembered: a busy client must not pay a Jupyter round trip per call.
func TestCodeAvailabilityIsCachedOnceUp(t *testing.T) {
	f := newMiniJupyter(t)
	s := newTestServer(t, Options{Jupyter: f.engine()})

	for range 5 {
		if status, _, body := s.do("GET", "/code/contexts", nil); status != http.StatusOK {
			t.Fatalf("list: %d %s", status, body)
		}
	}

	if n := f.specHits.Load(); n != 1 {
		t.Fatalf("Jupyter probed %d times for 5 calls, want once", n)
	}
}

// The first call can arrive while the image's entrypoint is still starting Jupyter: it waits for
// it instead of answering 501 for a server that is seconds away.
func TestCodeWaitsForAJupyterThatIsStarting(t *testing.T) {
	f := newMiniJupyter(t)
	f.downUntil.Store(time.Now().Add(700 * time.Millisecond).UnixNano())

	s := newTestServer(t, Options{Jupyter: f.engine(), JupyterStartupWait: 10 * time.Second})

	status, _, body := s.do("POST", "/code/context", map[string]string{"language": "python"})
	if status != http.StatusOK {
		t.Fatalf("create during startup: %d %s, want it to wait for Jupyter", status, body)
	}
}

// A configured Jupyter that never comes up is still a 501 - after the window, not forever.
func TestCodeGivesUpOnAJupyterThatNeverStarts(t *testing.T) {
	f := newMiniJupyter(t)
	f.downUntil.Store(time.Now().Add(time.Hour).UnixNano())

	s := newTestServer(t, Options{Jupyter: f.engine(), JupyterStartupWait: 400 * time.Millisecond})

	start := time.Now()
	status, _, body := s.do("GET", "/code/contexts", nil)
	wantError(t, status, body, http.StatusNotImplemented, codeNotSupported)

	if d := time.Since(start); d < 400*time.Millisecond || d > 5*time.Second {
		t.Fatalf("gave up after %s, want about the 400ms window", d)
	}
}

// After the engine reports Jupyter unreachable, the cache is dropped and the next call probes.
func TestCodeReprobesAfterJupyterGoesAway(t *testing.T) {
	f := newMiniJupyter(t)
	s := newTestServer(t, Options{Jupyter: f.engine(), JupyterStartupWait: 200 * time.Millisecond})

	if status, _, body := s.do("GET", "/code/contexts", nil); status != http.StatusOK {
		t.Fatalf("list: %d %s", status, body)
	}

	f.srv.Close()

	// Uses the cache, reaches the engine, which cannot reach Jupyter.
	if status, _, _ := s.do("POST", "/code/context", map[string]string{"language": "python"}); status == http.StatusOK {
		t.Fatal("create succeeded against a closed Jupyter")
	}

	// Now the probe runs again and finds nothing.
	status, _, body := s.do("GET", "/code/contexts", nil)
	wantError(t, status, body, http.StatusNotImplemented, codeNotSupported)
}

// Unconfigured is not "starting": no wait at all.
func TestCodeUnconfiguredDoesNotWait(t *testing.T) {
	s := newTestServer(t, Options{Jupyter: jupyter.New(jupyter.Config{}), JupyterStartupWait: 10 * time.Second})

	start := time.Now()
	status, _, body := s.do("GET", "/code/contexts", nil)
	wantError(t, status, body, http.StatusNotImplemented, codeNotSupported)

	if d := time.Since(start); d > time.Second {
		t.Fatalf("an image with no Jupyter waited %s", d)
	}
}
