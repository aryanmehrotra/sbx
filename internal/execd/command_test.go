//go:build unix

package execd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestForegroundCommand(t *testing.T) {
	s := newTestServer(t, Options{})
	dir := t.TempDir()

	cases := []struct {
		name       string
		body       map[string]any
		wantCode   int
		wantStdout []string
		wantStderr []string
	}{
		{"echo", map[string]any{"command": "echo hello-from-execd"}, 0, []string{"hello-from-execd"}, nil},
		{"two lines", map[string]any{"command": "echo line1 && echo line2"}, 0, []string{"line1", "line2"}, nil},
		{"exit code", map[string]any{"command": "exit 3"}, 3, nil, nil},
		{"stderr", map[string]any{"command": "echo oops >&2"}, 0, nil, []string{"oops"}},
		{"envs", map[string]any{"command": "echo $CUSTOM_VAR", "envs": map[string]string{"CUSTOM_VAR": "injected"}}, 0, []string{"injected"}, nil},
		{"cwd", map[string]any{"command": "pwd -P", "cwd": dir}, 0, []string{mustEval(t, dir)}, nil},
		{"cwd from env", map[string]any{"command": "pwd -P", "cwd": "$WHERE", "envs": map[string]string{"WHERE": dir}}, 0, []string{mustEval(t, dir)}, nil},
		{"argv is not shell-expanded", map[string]any{"argv": []string{"echo", "$HOME", "a b"}}, 0, []string{"$HOME a b"}, nil},
		{"no trailing newline", map[string]any{"command": "printf abc"}, 0, []string{"abc"}, nil},
		{"blank line is kept", map[string]any{"command": "printf 'a\\n\\nb\\n'"}, 0, []string{"a", "\n", "b"}, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exec := s.run(c.body)

			if len(exec.ID) != 32 {
				t.Errorf("init id %q, want 32 hex characters", exec.ID)
			}

			if exec.ExitCode == nil || *exec.ExitCode != c.wantCode {
				t.Fatalf("exit code %v, want %d (events %v, error %s=%s)", exec.ExitCode, c.wantCode, exec.Types, exec.ErrName, exec.ErrValue)
			}

			if c.wantCode == 0 && !exec.Complete {
				t.Errorf("no execution_complete event: %v", exec.Types)
			}

			if c.wantCode != 0 && (exec.Complete || exec.ErrName != "CommandExecError") {
				t.Errorf("a failed command must end in one CommandExecError, not complete: %v", exec.Types)
			}

			if c.wantStdout != nil && strings.Join(exec.Stdout, "|") != strings.Join(c.wantStdout, "|") {
				t.Errorf("stdout %q, want %q", exec.Stdout, c.wantStdout)
			}

			if c.wantStderr != nil && strings.Join(exec.Stderr, "|") != strings.Join(c.wantStderr, "|") {
				t.Errorf("stderr %q, want %q", exec.Stderr, c.wantStderr)
			}

			if exec.Types[0] != evInit {
				t.Errorf("first event is %q, want init", exec.Types[0])
			}
		})
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()

	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

// TestSSEFraming pins the wire format the SDKs depend on: bare JSON objects separated by a
// blank line, each with a type and a millisecond timestamp, and the headers that stop proxies
// buffering.
func TestSSEFraming(t *testing.T) {
	s := newTestServer(t, Options{})

	status, h, body := s.do("POST", "/command", map[string]any{"command": "echo framed"})
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}

	for k, v := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}

	if !bytes.HasSuffix(body, []byte("}\n\n")) {
		t.Fatalf("stream does not end with an event and a blank line: %q", body)
	}

	chunks := strings.Split(strings.TrimSuffix(string(body), "\n\n"), "\n\n")
	if len(chunks) != 3 {
		t.Fatalf("want init, stdout, execution_complete; got %d chunks: %q", len(chunks), body)
	}

	now := time.Now().UnixMilli()

	for i, want := range []string{"init", "stdout", "execution_complete"} {
		if strings.HasPrefix(chunks[i], "data:") || !strings.HasPrefix(chunks[i], "{") {
			t.Fatalf("event %d is not a bare JSON object: %q", i, chunks[i])
		}

		var ev map[string]any
		if err := json.Unmarshal([]byte(chunks[i]), &ev); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}

		if ev["type"] != want {
			t.Errorf("event %d type %v, want %s", i, ev["type"], want)
		}

		ts, _ := ev["timestamp"].(float64)
		if int64(ts) < now-60_000 || int64(ts) > now+1000 {
			t.Errorf("event %d timestamp %v is not unix milliseconds", i, ev["timestamp"])
		}
	}
}

func TestPingEventsAreIgnoredByTheSDK(t *testing.T) {
	old := pingInterval
	pingInterval = 50 * time.Millisecond

	t.Cleanup(func() { pingInterval = old })

	s := newTestServer(t, Options{})

	exec := s.run(map[string]any{"command": "sleep 0.3; echo after"})
	if exec.Pings == 0 {
		t.Fatalf("no ping events during a quiet command: %v", exec.Types)
	}

	if exec.Text() != "after" || *exec.ExitCode != 0 {
		t.Fatalf("pings leaked into output or exit: %q %v", exec.Stdout, *exec.ExitCode)
	}
}

func TestCommandRequestValidation(t *testing.T) {
	s := newTestServer(t, Options{})

	cases := []struct {
		name string
		body string
	}{
		{"not json", `{`},
		{"neither", `{"cwd":"/"}`},
		{"both", `{"command":"true","argv":["true"]}`},
		{"empty command", `{"command":""}`},
		{"empty argv", `{"argv":[]}`},
		{"null argv element", `{"argv":["echo",null]}`},
		{"empty argv0", `{"argv":[""]}`},
		{"NUL in argv", `{"argv":["echo","a\u0000b"]}`},
		{"negative timeout", `{"command":"true","timeout":-1}`},
		{"gid without uid", `{"command":"true","gid":0}`},
		{"missing cwd", `{"command":"true","cwd":"/definitely/not/a/dir"}`},
		{"cwd is a file", `{"command":"true","cwd":"/etc/hosts"}`},
		{"undefined variable in cwd", `{"command":"true","cwd":"$SBX_EXECD_NOT_SET/x"}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, _, body := s.do("POST", "/command", c.body)
			wantError(t, status, body, http.StatusBadRequest, codeInvalidRequest)
		})
	}
}

func TestStartFailureIsReportedInTheStream(t *testing.T) {
	s := newTestServer(t, Options{})

	exec := s.run(map[string]any{"argv": []string{"sbx-execd-no-such-binary"}})
	if exec.ID == "" || exec.ErrName != "CommandExecError" || !strings.Contains(exec.ErrValue, "not found") {
		t.Fatalf("want init then CommandExecError naming the missing binary, got %+v", exec)
	}
}

func TestForegroundTimeoutKillsTheGroup(t *testing.T) {
	s := newTestServer(t, Options{})
	pidFile := filepath.Join(t.TempDir(), "pid")

	start := time.Now()
	exec := s.run(map[string]any{
		"command": "sleep 30 & echo $! > " + pidFile + "; wait",
		"timeout": 300,
	})

	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout of 300ms took %s", time.Since(start))
	}

	if exec.ExitCode == nil || *exec.ExitCode != 128+int(syscall.SIGKILL) {
		t.Fatalf("exit code %v, want %d (killed)", exec.ExitCode, 128+int(syscall.SIGKILL))
	}

	assertDead(t, pidFile)

	status := s.status(exec.ID)
	if status.Running || !strings.Contains(status.Error, "timeout") {
		t.Fatalf("status after timeout: %+v", status)
	}
}

// assertDead reads a pid from file and waits for that process to be gone - proof the signal
// reached the process group, not just the shell.
func assertDead(t *testing.T, pidFile string) {
	t.Helper()

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}

	eventually(t, 5*time.Second, "grandchild "+strconv.Itoa(pid)+" exiting", func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
}

func (s *testServer) status(id string) statusResponse {
	s.t.Helper()

	code, _, body := s.do("GET", "/command/status/"+id, nil)
	if code != http.StatusOK {
		s.t.Fatalf("status of %s: %d %s", id, code, body)
	}

	var st statusResponse
	if err := json.Unmarshal(body, &st); err != nil {
		s.t.Fatal(err)
	}

	return st
}

func (s *testServer) logs(id string, cursor string) (string, int64) {
	s.t.Helper()

	path := "/command/" + id + "/logs"
	if cursor != "" {
		path += "?cursor=" + cursor
	}

	code, h, body := s.do("GET", path, nil)
	if code != http.StatusOK {
		s.t.Fatalf("logs of %s: %d %s", id, code, body)
	}

	if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		s.t.Fatalf("logs Content-Type %q, want text/plain", ct)
	}

	n, err := strconv.ParseInt(h.Get("EXECD-COMMANDS-TAIL-CURSOR"), 10, 64)
	if err != nil {
		s.t.Fatalf("EXECD-COMMANDS-TAIL-CURSOR %q: %v", h.Get("EXECD-COMMANDS-TAIL-CURSOR"), err)
	}

	return string(body), n
}

func TestBackgroundCommandStatusAndLogs(t *testing.T) {
	s := newTestServer(t, Options{})

	exec := s.run(map[string]any{"command": "echo bg-start; sleep 0.4; echo bg-end >&2; exit 4", "background": true})
	if exec.ID == "" || !exec.Complete || exec.ExitCode == nil || *exec.ExitCode != 0 {
		t.Fatalf("a background command answers init + execution_complete at once, got %+v", exec)
	}

	st := s.status(exec.ID)
	if st.ID != exec.ID || !st.Running || st.ExitCode != nil || st.StartedAt.IsZero() || st.FinishedAt != nil {
		t.Fatalf("status while running: %+v", st)
	}

	if !strings.Contains(st.Content, "bg-start") {
		t.Errorf("status content %q is not the command", st.Content)
	}

	eventually(t, 5*time.Second, "bg-start in the logs", func() bool {
		out, _ := s.logs(exec.ID, "")
		return strings.Contains(out, "bg-start")
	})

	eventually(t, 5*time.Second, "the background command finishing", func() bool { return !s.status(exec.ID).Running })

	st = s.status(exec.ID)
	if st.ExitCode == nil || *st.ExitCode != 4 || st.FinishedAt == nil || st.Error != "exit status 4" {
		t.Fatalf("status after exit: %+v", st)
	}

	all, cursor := s.logs(exec.ID, "")
	if all != "bg-start\nbg-end\n" || cursor != int64(len(all)) {
		t.Fatalf("logs %q cursor %d; want stdout and stderr combined and the cursor at the end", all, cursor)
	}

	tail, next := s.logs(exec.ID, strconv.Itoa(len("bg-start\n")))
	if tail != "bg-end\n" || next != cursor {
		t.Fatalf("logs from cursor: %q, next %d", tail, next)
	}

	past, next := s.logs(exec.ID, "99999")
	if past != "" || next != cursor {
		t.Fatalf("a cursor past the end should read nothing and return the end: %q %d", past, next)
	}

	code, _, body := s.do("GET", "/command/"+exec.ID+"/logs?cursor=-1", nil)
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)
}

func TestCommandLookupErrors(t *testing.T) {
	s := newTestServer(t, Options{})

	code, _, body := s.do("GET", "/command/status/nope", nil)
	wantError(t, code, body, http.StatusNotFound, codeInvalidRequest)

	code, _, body = s.do("GET", "/command/nope/logs", nil)
	wantError(t, code, body, http.StatusNotFound, codeInvalidRequest)

	fg := s.run(map[string]any{"command": "true"})
	code, _, body = s.do("GET", "/command/"+fg.ID+"/logs", nil)
	wantError(t, code, body, http.StatusBadRequest, codeInvalidRequest)

	if st := s.status(fg.ID); st.Running || st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("a foreground command is in the status table too: %+v", st)
	}

	code, _, body = s.do("DELETE", "/command", nil)
	wantError(t, code, body, http.StatusBadRequest, codeMissingQuery)

	code, _, body = s.do("DELETE", "/command?id=nope", nil)
	wantError(t, code, body, http.StatusNotFound, codeContextNotFound)

	code, _, body = s.do("DELETE", "/command?id="+fg.ID, nil)
	wantError(t, code, body, http.StatusInternalServerError, codeRuntimeError)
}

// TestInterruptKillsTheProcessGroup proves DELETE /command reaches a grandchild the command's
// shell started, not only the shell.
func TestInterruptKillsTheProcessGroup(t *testing.T) {
	s := newTestServer(t, Options{})
	pidFile := filepath.Join(t.TempDir(), "pid")

	exec := s.run(map[string]any{"command": "sleep 300 & echo $! > " + pidFile + "; wait", "background": true})

	eventually(t, 5*time.Second, "the grandchild's pid file", func() bool {
		b, err := os.ReadFile(pidFile)
		return err == nil && len(bytes.TrimSpace(b)) > 0
	})

	start := time.Now()

	code, _, body := s.do("DELETE", "/command?id="+exec.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("interrupt: %d %s", code, body)
	}

	if time.Since(start) > 2*time.Second {
		t.Errorf("interrupt of a SIGTERM-honouring group took %s; it should not wait out the SIGKILL grace", time.Since(start))
	}

	assertDead(t, pidFile)

	eventually(t, 5*time.Second, "status showing it stopped", func() bool { return !s.status(exec.ID).Running })

	if st := s.status(exec.ID); st.ExitCode == nil || *st.ExitCode != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code after SIGTERM: %+v", st)
	}
}

func TestInterruptEscalatesToSIGKILL(t *testing.T) {
	old := interruptGrace
	interruptGrace = 200 * time.Millisecond

	t.Cleanup(func() { interruptGrace = old })

	s := newTestServer(t, Options{})
	ready := filepath.Join(t.TempDir(), "ready")

	exec := s.run(map[string]any{"command": "trap '' TERM; touch " + ready + "; while :; do sleep 0.05; done", "background": true})

	eventually(t, 5*time.Second, "the trap being installed", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})

	if code, _, body := s.do("DELETE", "/command?id="+exec.ID, nil); code != http.StatusOK {
		t.Fatalf("interrupt: %d %s", code, body)
	}

	eventually(t, 5*time.Second, "SIGKILL ending a SIGTERM-ignoring command", func() bool { return !s.status(exec.ID).Running })
}

func TestClientDisconnectKillsForegroundCommand(t *testing.T) {
	s := newTestServer(t, Options{})
	pidFile := filepath.Join(t.TempDir(), "pid")

	req, _ := http.NewRequest("POST", s.ts.URL+"/command",
		strings.NewReader(`{"command":"sleep 300 & echo $! > `+pidFile+`; wait"}`))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	// Read the init event, so the command is certainly running, then hang up.
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	eventually(t, 5*time.Second, "the pid file", func() bool {
		b, err := os.ReadFile(pidFile)
		return err == nil && len(bytes.TrimSpace(b)) > 0
	})

	_ = resp.Body.Close()

	assertDead(t, pidFile)
}

func TestBackgroundTimeout(t *testing.T) {
	s := newTestServer(t, Options{})

	exec := s.run(map[string]any{"command": "sleep 30", "background": true, "timeout": 200})

	eventually(t, 5*time.Second, "the timeout ending it", func() bool { return !s.status(exec.ID).Running })

	if st := s.status(exec.ID); !strings.Contains(st.Error, "timeout") {
		t.Fatalf("status %+v does not say it timed out", st)
	}
}

func TestSweepDropsOnlyOldFinishedCommands(t *testing.T) {
	s := newTestServer(t, Options{})

	done := s.run(map[string]any{"command": "echo x", "background": true})
	running := s.run(map[string]any{"command": "sleep 30", "background": true})

	eventually(t, 5*time.Second, "the first finishing", func() bool { return !s.status(done.ID).Running })

	path := s.srv.lookup(done.ID).logPath
	s.srv.sweep(time.Now().Add(time.Hour))

	if s.srv.lookup(done.ID) != nil {
		t.Error("a finished command older than the cutoff was kept")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("its output file was kept: %v", err)
	}

	if s.srv.lookup(running.ID) == nil {
		t.Error("a running command was dropped")
	}
}

func TestEnvironment(t *testing.T) {
	t.Setenv(EnvAccessToken, "hidden")

	envFile := filepath.Join(t.TempDir(), "envs")
	if err := os.WriteFile(envFile, []byte("# comment\nFROM_FILE=file\nexport QUOTED=\"a b\"\nOVERRIDE=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvExtraEnvs, envFile)

	s := newTestServer(t, Options{})

	exec := s.run(map[string]any{
		"command": `echo "$FROM_FILE|$QUOTED|$OVERRIDE|${EXECD_ACCESS_TOKEN:-unset}"`,
		"envs":    map[string]string{"OVERRIDE": "request"},
	})

	if got := exec.Text(); got != "file|a b|request|unset" {
		t.Fatalf("got %q: want EXECD_ENVS layered under the request and the token hidden", got)
	}
}

func TestLineSplitter(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"a\nb\n"}, []string{"a", "b"}},
		{[]string{"a", "b\n"}, []string{"ab"}},
		{[]string{"a\r\nb\r\n"}, []string{"a", "b"}},
		{[]string{"a\r", "\nb"}, []string{"a", "b"}},
		{[]string{"progress 1\rprogress 2\r"}, []string{"progress 1", "progress 2"}},
		{[]string{"a\n\nb"}, []string{"a", "\n", "b"}},
		{[]string{"tail"}, []string{"tail"}},
	}

	for _, c := range cases {
		var got []string

		ls := &lineSplitter{emit: func(s string) { got = append(got, s) }}
		for _, chunk := range c.in {
			ls.write([]byte(chunk))
		}

		ls.flush()

		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}
