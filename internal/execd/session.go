//go:build unix

package execd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A bash session is upstream's design, not a long-lived shell: each run is a fresh shell that
// first re-applies the environment and directory the previous run left, runs the command, and
// then prints its exported environment and $PWD behind markers for the next run to pick up.
//
// That is what "state persists" means here - `cd` and `export` carry over; unexported variables,
// functions and aliases do not. It is chosen over one persistent shell because there is no
// reliable way to find where a command's output ends in a shell that never exits, and a session
// whose shell has wedged on a read from stdin is one no later request can recover.
const (
	markEnvStart = "__ENV_DUMP_START__"
	markEnvEnd   = "__ENV_DUMP_END__"
	markExit     = "__EXIT_CODE__:"
	markPwd      = "__PWD__:"
)

// sessionDefaultTimeout is upstream's cap for a run that names none.
const sessionDefaultTimeout = 24 * time.Hour

// maxPersistedValue drops enormous variables from what a session carries forward, as upstream
// does; they would be re-exported by every later run.
const maxPersistedValue = 8 << 10

// notPersisted are the prompt variables, which only make sense to an interactive shell.
var notPersisted = map[string]bool{"PS1": true, "PS2": true, "PS3": true, "PS4": true, "PROMPT_COMMAND": true}

type session struct {
	id string

	mu      sync.Mutex
	env     map[string]string
	cwd     string
	pgid    int
	deleted bool
}

func (ss *session) currentGroup() int {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	return ss.pgid
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cwd string `json:"cwd"`
	}

	// The body is optional: none, or {}, means defaults.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid session request: "+err.Error())
		return
	}

	env := userEnv(nil)

	cwd, err := expandPath(req.Cwd, env)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid session request: "+err.Error())
		return
	}

	// Upstream creates the session's directory rather than refusing a missing one.
	if cwd != "" {
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, codeRuntimeError, fmt.Sprintf("create session directory %s: %v", cwd, err))
			return
		}
	}

	ss := &session{id: newID(), env: env, cwd: cwd}

	s.mu.Lock()
	s.sessions[ss.id] = ss
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{"session_id": ss.id})
}

func (s *Server) getSession(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.sessions[id]
}

// closeSession kills whatever the session is running and forgets it. It reports whether the
// session existed.
func (s *Server) closeSession(id string) bool {
	s.mu.Lock()
	ss := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()

	if ss == nil {
		return false
	}

	ss.mu.Lock()
	pgid := ss.pgid
	ss.deleted = true
	ss.mu.Unlock()

	if pgid > 0 {
		_ = signalGroup(pgid, syscall.SIGKILL)
	}

	return true
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sessionId")

	if !s.closeSession(id) {
		writeError(w, http.StatusNotFound, codeContextNotFound, fmt.Sprintf("session %s not found; it may already be deleted", id))
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) runInSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sessionId")

	var req struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Timeout int64  `json:"timeout"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid run request: "+err.Error())
		return
	}

	if req.Command == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid run request: command is required")
		return
	}

	if req.Timeout < 0 {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid run request: timeout must not be negative")
		return
	}

	ss := s.getSession(id)
	if ss == nil {
		writeError(w, http.StatusNotFound, codeSessionNotFound,
			fmt.Sprintf("session %s not found; create one with POST /session", id))

		return
	}

	ss.mu.Lock()
	env := make(map[string]string, len(ss.env))
	for k, v := range ss.env {
		env[k] = v
	}

	cwd := ss.cwd
	ss.mu.Unlock()

	if req.Cwd != "" {
		dir, err := resolveCwd(req.Cwd, env)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid run request: "+err.Error())
			return
		}

		cwd = dir
	}

	script, err := os.CreateTemp("", "sbx-execd-session-*.sh")
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeRuntimeError, "write session script: "+err.Error())
		return
	}

	defer os.Remove(script.Name())

	_, werr := script.WriteString(wrapScript(req.Command, env, cwd))
	if cerr := script.Close(); werr == nil {
		werr = cerr
	}

	if werr != nil {
		writeError(w, http.StatusInternalServerError, codeRuntimeError, "write session script: "+werr.Error())
		return
	}

	args := []string{script.Name()}
	if shell() == "bash" {
		args = []string{"--noprofile", "--norc", script.Name()}
	}

	cmd := exec.Command(shell(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The shell starts from the session's environment rather than execd's, so a variable an
	// earlier run unset stays unset. Upstream inherits execd's here, and `unset` lasts one run.
	cmd.Env = envList(env)

	outR, outW, err := os.Pipe()
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeRuntimeError, "create output pipe: "+err.Error())
		return
	}
	defer outR.Close()

	cmd.Stdout, cmd.Stderr = outW, outW

	st := newStream(w)
	defer st.stop()

	started := time.Now()
	p, err := s.procs.start(cmd)
	_ = outW.Close()

	if err != nil {
		writeError(w, http.StatusInternalServerError, codeRuntimeError, fmt.Sprintf("start %s for session %s: %v", shell(), id, err))
		return
	}

	ss.mu.Lock()
	ss.pgid = p.pid
	ss.mu.Unlock()

	defer func() {
		ss.mu.Lock()
		if ss.pgid == p.pid {
			ss.pgid = 0
		}
		ss.mu.Unlock()
	}()

	// Upstream's init event carries the session id, not a per-run id.
	st.send(event{Type: evInit, Text: id})
	st.startPing()

	var (
		envLines []string
		pwd      string
		marked   *int
		readDone = make(chan struct{})
	)

	go func() {
		defer close(readDone)

		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 64<<10), 16<<20)

		inEnv := false

		for sc.Scan() {
			line := sc.Text()

			switch {
			case line == markEnvStart:
				inEnv = true
			case line == markEnvEnd:
				inEnv = false
			case strings.HasPrefix(line, markExit):
				if n, err := strconv.Atoi(strings.TrimPrefix(line, markExit)); err == nil {
					marked = &n
				}
			case strings.HasPrefix(line, markPwd):
				pwd = strings.TrimPrefix(line, markPwd)
			case inEnv:
				envLines = append(envLines, line)
			case line != "":
				st.send(event{Type: evStdout, Text: line})
			}
		}
	}()

	timeout := time.Duration(req.Timeout) * time.Millisecond
	if timeout <= 0 {
		timeout = sessionDefaultTimeout
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var why string

	select {
	case <-p.done:
	case <-r.Context().Done():
		why = "client disconnected"
		_ = signalGroup(p.pid, syscall.SIGKILL)
		<-p.done
	case <-timer.C:
		why = fmt.Sprintf("timeout after %s", timeout)
		_ = signalGroup(p.pid, syscall.SIGKILL)
		<-p.done
	}

	_ = outR.SetReadDeadline(time.Now().Add(drainGrace))
	<-readDone

	ss.mu.Lock()
	if !ss.deleted && why == "" {
		if next := parseExportDump(envLines); len(next) > 0 {
			ss.env = next
		}

		if pwd != "" {
			ss.cwd = pwd
		}
	}
	ss.mu.Unlock()

	code := p.exitCode()
	if marked != nil && why == "" {
		code = *marked
	}

	if code == 0 && why == "" {
		st.send(event{Type: evComplete, ExecutionTime: time.Since(started).Milliseconds()})
		return
	}

	msg := fmt.Sprintf("command exited with code %d", code)
	if why != "" {
		msg = p.describe() + " (" + why + ")"
	}

	st.send(errorEvent("CommandExecError", strconv.Itoa(code), msg))
}

// wrapScript is upstream's buildWrappedScript.
func wrapScript(command string, env map[string]string, cwd string) string {
	var b strings.Builder

	keys := make([]string, 0, len(env))
	for k, v := range env {
		if validEnvKey(k) && !notPersisted[k] && len(v) <= maxPersistedValue {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	for _, k := range keys {
		b.WriteString("export " + k + "=" + shellQuote(env[k]) + "\n")
	}

	if cwd != "" {
		b.WriteString("cd " + shellQuote(cwd) + "\n")
	}

	b.WriteString(command)

	if !strings.HasSuffix(command, "\n") {
		b.WriteString("\n")
	}

	// The leading \n puts the marker on its own line even when the command's last output had
	// no newline; the blank line it may leave is dropped by the reader.
	b.WriteString("__USER_EXIT_CODE__=$?\n")
	b.WriteString(`printf "\n%s\n" "` + markEnvStart + "\"\n")
	b.WriteString("export -p\n")
	b.WriteString(`printf "%s\n" "` + markEnvEnd + "\"\n")
	b.WriteString(`printf "` + markPwd + `%s\n" "$(pwd)"` + "\n")
	b.WriteString(`printf "` + markExit + `%s\n" "$__USER_EXIT_CODE__"` + "\n")
	b.WriteString("exit \"$__USER_EXIT_CODE__\"\n")

	return b.String()
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'"
}

// parseExportDump reads `export -p` from bash ("declare -x K="v"") or a POSIX sh
// ("export K='v'").
func parseExportDump(lines []string) map[string]string {
	if len(lines) == 0 {
		return nil
	}

	env := make(map[string]string, len(lines))

	for _, line := range lines {
		k, v, ok := parseExportLine(line)
		if !ok || notPersisted[k] || len(v) > maxPersistedValue {
			continue
		}

		env[k] = v
	}

	return env
}

func parseExportLine(line string) (string, string, bool) {
	var rest string

	switch {
	case strings.HasPrefix(line, "declare -x "):
		rest = strings.TrimPrefix(line, "declare -x ")
	case strings.HasPrefix(line, "export "):
		rest = strings.TrimPrefix(line, "export ")
	default:
		return "", "", false
	}

	rest = strings.TrimSpace(rest)

	name, raw, hasValue := strings.Cut(rest, "=")
	if !validEnvKey(name) {
		return "", "", false
	}

	if !hasValue {
		return name, "", true
	}

	if v, err := strconv.Unquote(raw); err == nil {
		return name, v, true
	}

	if v, ok := unquoteShellWord(raw); ok {
		return name, v, true
	}

	return name, strings.Trim(raw, `"`), true
}

// unquoteShellWord undoes the quoting `export -p` applies: single quotes literal, double quotes
// with \\ \" \$ \` escapes, and bare backslash escapes.
func unquoteShellWord(raw string) (string, bool) {
	var b strings.Builder

	for i := 0; i < len(raw); {
		switch raw[i] {
		case '\'':
			end := strings.IndexByte(raw[i+1:], '\'')
			if end < 0 {
				return "", false
			}

			b.WriteString(raw[i+1 : i+1+end])
			i += end + 2
		case '"':
			j := i + 1
			for ; j < len(raw) && raw[j] != '"'; j++ {
				if raw[j] == '\\' && j+1 < len(raw) && strings.IndexByte("\\\"$`", raw[j+1]) >= 0 {
					j++
				}

				b.WriteByte(raw[j])
			}

			if j >= len(raw) {
				return "", false
			}

			i = j + 1
		case '\\':
			if i+1 >= len(raw) {
				return "", false
			}

			b.WriteByte(raw[i+1])
			i += 2
		default:
			b.WriteByte(raw[i])
			i++
		}
	}

	return b.String(), true
}
