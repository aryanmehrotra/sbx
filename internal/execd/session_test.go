//go:build unix

package execd

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func (s *testServer) newSession(body any) string {
	s.t.Helper()

	code, _, data := s.do("POST", "/session", body)
	if code != http.StatusOK {
		s.t.Fatalf("create session: %d %s", code, data)
	}

	var resp struct {
		SessionID string `json:"session_id"`
	}

	if err := json.Unmarshal(data, &resp); err != nil || resp.SessionID == "" {
		s.t.Fatalf("create session answered %s", data)
	}

	return resp.SessionID
}

func TestSessionStatePersists(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := mustEval(t, t.TempDir())

	id := s.newSession(struct{}{})

	first := s.stream("POST", "/session/"+id+"/run", map[string]any{
		"command": "cd " + dir + " && export SESSION_VAR=hello_session && NOT_EXPORTED=x",
	})
	if first.ID != id || first.ExitCode == nil || *first.ExitCode != 0 {
		t.Fatalf("first run: %+v", first)
	}

	second := s.stream("POST", "/session/"+id+"/run", map[string]any{
		"command": `pwd -P; echo "$SESSION_VAR"; echo "${NOT_EXPORTED:-gone}"`,
	})

	want := []string{dir, "hello_session", "gone"}
	if len(second.Stdout) != 3 || second.Stdout[0] != want[0] || second.Stdout[1] != want[1] || second.Stdout[2] != want[2] {
		t.Fatalf("second run saw %q, want %q: cd and export persist, a plain assignment does not", second.Stdout, want)
	}

	third := s.stream("POST", "/session/"+id+"/run", map[string]any{"command": "unset SESSION_VAR"})
	if *third.ExitCode != 0 {
		t.Fatalf("unset failed: %+v", third)
	}

	fourth := s.stream("POST", "/session/"+id+"/run", map[string]any{"command": `echo "${SESSION_VAR:-unset}"`})
	if fourth.Text() != "unset" {
		t.Fatalf("an unset variable came back: %q", fourth.Text())
	}
}

func TestSessionRunDetails(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := mustEval(t, t.TempDir())

	id := s.newSession(map[string]string{"cwd": filepath.Join(dir, "made")})

	exec := s.stream("POST", "/session/"+id+"/run", map[string]any{"command": "pwd -P"})
	if exec.Text() != filepath.Join(dir, "made") {
		t.Fatalf("session cwd: %q (a missing directory is created, as upstream)", exec.Text())
	}

	exec = s.stream("POST", "/session/"+id+"/run", map[string]any{"command": "pwd -P", "cwd": dir})
	if exec.Text() != dir {
		t.Fatalf("per-run cwd override: %q", exec.Text())
	}

	exec = s.stream("POST", "/session/"+id+"/run", map[string]any{"command": "echo out; echo err >&2; exit 5"})
	if exec.ExitCode == nil || *exec.ExitCode != 5 || exec.Complete {
		t.Fatalf("a failing run: %+v", exec)
	}

	if len(exec.Stdout) != 2 {
		t.Errorf("a session merges stderr into stdout, as upstream; got %q", exec.Stdout)
	}

	exec = s.stream("POST", "/session/"+id+"/run", map[string]any{"command": "printf no-newline"})
	if exec.Text() != "no-newline" {
		t.Fatalf("output without a trailing newline: %q", exec.Stdout)
	}

	start := time.Now()
	exec = s.stream("POST", "/session/"+id+"/run", map[string]any{"command": "sleep 30", "timeout": 200})

	if time.Since(start) > 10*time.Second || exec.ExitCode == nil || *exec.ExitCode == 0 {
		t.Fatalf("a timed-out run: %+v after %s", exec, time.Since(start))
	}

	exec = s.stream("POST", "/session/"+id+"/run", map[string]any{"command": "echo still-usable"})
	if exec.Text() != "still-usable" {
		t.Fatalf("the session after a timeout: %+v", exec)
	}
}

func TestSessionErrors(t *testing.T) {
	s := newTestServer(t, Options{})

	code, _, body := s.do("POST", "/session", "{not json")
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	// No body at all is allowed.
	code, _, body = s.do("POST", "/session", nil)
	if code != http.StatusOK {
		t.Fatalf("empty body: %d %s", code, body)
	}

	code, _, body = s.do("POST", "/session/nope/run", map[string]any{"command": "true"})
	wantError(t, code, body, http.StatusNotFound, codeSessionNotFound)

	id := s.newSession(struct{}{})

	code, _, body = s.do("POST", "/session/"+id+"/run", map[string]any{"command": ""})
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	code, _, body = s.do("POST", "/session/"+id+"/run", map[string]any{"command": "true", "cwd": "/definitely/not/here"})
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	code, _, body = s.do("DELETE", "/session/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}

	code, _, body = s.do("DELETE", "/session/"+id, nil)
	wantError(t, code, body, http.StatusNotFound, codeContextNotFound)

	code, _, body = s.do("POST", "/session/"+id+"/run", map[string]any{"command": "true"})
	wantError(t, code, body, http.StatusNotFound, codeSessionNotFound)
}

func TestDeletingASessionKillsItsRun(t *testing.T) {
	s := newTestServer(t, Options{})
	id := s.newSession(struct{}{})
	pidFile := filepath.Join(t.TempDir(), "pid")

	done := make(chan *sdkExecution, 1)

	go func() {
		done <- s.stream("POST", "/session/"+id+"/run", map[string]any{"command": "sleep 300 & echo $! > " + pidFile + "; wait"})
	}()

	eventually(t, 5*time.Second, "the run starting", func() bool {
		_, err := os.Stat(pidFile)
		return err == nil && s.srv.getSession(id).currentGroup() > 0
	})

	// DELETE /command accepts a session id too, as upstream's Interrupt does.
	if code, _, body := s.do("DELETE", "/command?id="+id, nil); code != http.StatusOK {
		t.Fatalf("interrupt session: %d %s", code, body)
	}

	select {
	case exec := <-done:
		if exec.ExitCode == nil || *exec.ExitCode == 0 {
			t.Fatalf("the killed run reported success: %+v", exec)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end when its session was deleted")
	}

	assertDead(t, pidFile)
}

func TestParseExportLine(t *testing.T) {
	cases := []struct{ in, k, v string }{
		{`declare -x FOO="bar"`, "FOO", "bar"},
		{`declare -x FOO="a \"q\" \$x"`, "FOO", `a "q" $x`},
		{`declare -x EMPTY`, "EMPTY", ""},
		{`export FOO='it'"'"'s'`, "FOO", "it's"},
		{`export PATH='/usr/bin:/bin'`, "PATH", "/usr/bin:/bin"},
	}

	for _, c := range cases {
		k, v, ok := parseExportLine(c.in)
		if !ok || k != c.k || v != c.v {
			t.Errorf("%s: got %q=%q (%v), want %q=%q", c.in, k, v, ok, c.k, c.v)
		}
	}

	if _, _, ok := parseExportLine("not an export"); ok {
		t.Error("parsed a line that is not an export")
	}
}
