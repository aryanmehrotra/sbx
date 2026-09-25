//go:build unix

package execd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// commandRetention is the spec's "retained for at least 24 hours" for finished commands.
const commandRetention = 24 * time.Hour

// drainGrace is how long output is still read after a foreground command's process has exited.
// A pipe only reaches EOF when every holder of its write end is gone, and `cmd &` inside the
// command leaves a grandchild holding it - upstream tails files, so it never waits for that
// grandchild, and neither may this. What the exited process wrote is already in the pipe.
var drainGrace = 100 * time.Millisecond

// interruptGrace is upstream's SIGTERM-to-SIGKILL window for DELETE /command.
var interruptGrace = 3 * time.Second

// command is one entry in the status table: foreground or background, running or finished.
type command struct {
	id         string
	content    string
	background bool
	logPath    string
	startedAt  time.Time

	mu         sync.Mutex
	pgid       int
	running    bool
	exitCode   *int
	errMsg     string
	finishedAt *time.Time
}

func (c *command) runningGroup() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return 0
	}

	return c.pgid
}

func (c *command) finish(code int, msg string) {
	now := time.Now()

	c.mu.Lock()
	c.running = false
	c.exitCode = &code
	c.errMsg = msg
	c.finishedAt = &now
	c.pgid = 0
	c.mu.Unlock()
}

// runCommandRequest is RunCommandRequest. uid and gid are pointers because 0 is root, not
// "unset".
type runCommandRequest struct {
	Command    string            `json:"command"`
	Argv       []string          `json:"argv"`
	Cwd        string            `json:"cwd"`
	Background bool              `json:"background"`
	Timeout    int64             `json:"timeout"`
	UID        *int64            `json:"uid"`
	GID        *int64            `json:"gid"`
	Envs       map[string]string `json:"envs"`
}

// decodeRunCommand applies upstream's rules (model.RunCommandRequest): exactly one of a
// non-empty command or argv, argv[0] non-empty, no NUL, no null argv elements, a non-negative
// timeout, and gid only together with uid.
func decodeRunCommand(body io.Reader) (*runCommandRequest, error) {
	data, err := io.ReadAll(io.LimitReader(body, 16<<20))
	if err != nil {
		return nil, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}

	var req runCommandRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, err
	}

	_, hasCommand := fields["command"]
	_, hasArgv := fields["argv"]

	if hasCommand == hasArgv || hasCommand && req.Command == "" || hasArgv && req.Argv == nil {
		return nil, errors.New("exactly one of a non-empty command or argv is required")
	}

	if hasArgv {
		var raw []json.RawMessage
		if err := json.Unmarshal(fields["argv"], &raw); err != nil {
			return nil, err
		}

		for _, a := range raw {
			if string(a) == "null" {
				return nil, errors.New("argv elements must be strings")
			}
		}

		if len(req.Argv) == 0 || req.Argv[0] == "" {
			return nil, errors.New("argv must start with a non-empty executable")
		}

		for _, a := range req.Argv {
			if strings.ContainsRune(a, 0) {
				return nil, errors.New("argv must not contain NUL")
			}
		}
	}

	if req.Timeout < 0 {
		return nil, errors.New("timeout must be a positive number of milliseconds, or omitted for none")
	}

	if req.GID != nil && req.UID == nil {
		return nil, errors.New("uid is required when gid is provided")
	}

	for _, id := range []*int64{req.UID, req.GID} {
		if id != nil && (*id < 0 || *id > 1<<32-1) {
			return nil, fmt.Errorf("uid/gid %d is out of range", *id)
		}
	}

	return &req, nil
}

func (r *runCommandRequest) content() string {
	if r.Argv == nil {
		return r.Command
	}

	b, _ := json.Marshal(r.Argv)

	return string(b)
}

// shell is bash when the image has it and sh otherwise, the choice upstream makes once.
var shell = sync.OnceValue(func() string {
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash"
	}

	return "sh"
})

// resolveCwd expands and checks a working directory, so a typo answers 400 naming the
// directory instead of a stream that fails in chdir.
func resolveCwd(cwd string, env map[string]string) (string, error) {
	dir, err := expandPath(cwd, env)
	if err != nil || dir == "" {
		return dir, err
	}

	fi, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("working directory %s does not exist; create it first (POST /directories)", cwd)
		}

		return "", fmt.Errorf("cannot access working directory %s: %w", cwd, err)
	}

	if !fi.IsDir() {
		return "", fmt.Errorf("working directory %s is not a directory", cwd)
	}

	return dir, nil
}

// buildCmd turns a request into an exec.Cmd in its own process group, so an interrupt or a
// timeout reaches everything the command started and not just the shell.
func buildCmd(req *runCommandRequest) (*exec.Cmd, error) {
	env := userEnv(req.Envs)

	dir, err := resolveCwd(req.Cwd, env)
	if err != nil {
		return nil, err
	}

	if dir != "" {
		env["PWD"] = dir
	}

	var cmd *exec.Cmd

	if req.Argv != nil {
		path, err := lookArgv0(req.Argv[0], dir, env["PATH"])

		cmd = &exec.Cmd{Path: path, Args: append([]string(nil), req.Argv...), Err: err}
	} else {
		cmd = exec.Command(shell(), "-c", req.Command)
	}

	cmd.Dir = dir
	cmd.Env = envList(env)

	cred, err := credential(req.UID, req.GID)
	if err != nil {
		return nil, err
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}

	return cmd, nil
}

// lookArgv0 resolves argv[0] as the spec says: a path with a slash is relative to cwd, a bare
// name is searched in the child's PATH (absolute entries only) - not execd's own PATH, which is
// what exec.LookPath would use.
func lookArgv0(name, cwd, path string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}

	if strings.Contains(name, "/") {
		return filepath.Abs(filepath.Join(cwd, name))
	}

	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}

		candidate := filepath.Join(dir, name)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}

	return name, &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// credential is upstream's buildCredential: nil when the request asks for who execd already
// is, otherwise the uid with its primary group and supplementary groups from /etc/passwd and
// /etc/group, and gid overriding the primary group.
func credential(uid, gid *int64) (*syscall.Credential, error) {
	if uid == nil && gid == nil {
		return nil, nil
	}

	if int(*uid) == os.Getuid() && (gid == nil || int(*gid) == os.Getgid()) {
		return nil, nil
	}

	cred := &syscall.Credential{Uid: uint32(*uid), Gid: uint32(os.Getgid())}

	if u, err := user.LookupId(strconv.FormatInt(*uid, 10)); err == nil {
		if g, err := strconv.ParseUint(u.Gid, 10, 32); err == nil {
			cred.Gid = uint32(g)
		}

		if ids, err := u.GroupIds(); err == nil {
			for _, s := range ids {
				if g, err := strconv.ParseUint(s, 10, 32); err == nil {
					cred.Groups = append(cred.Groups, uint32(g))
				}
			}
		}
	}

	if gid != nil {
		cred.Gid = uint32(*gid)
	}

	return cred, nil
}

// startHint adds the likely cause to a start failure that is about identity, because "operation
// not permitted" alone sends people looking at file modes.
func startHint(err error, cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil || !errors.Is(err, os.ErrPermission) {
		return err
	}

	c := cmd.SysProcAttr.Credential

	return fmt.Errorf("%w (running as uid=%d gid=%d needs CAP_SETUID/CAP_SETGID, which this sandbox's "+
		"container does not have; drop uid/gid from the request or run execd as root)", err, c.Uid, c.Gid)
}

func (s *Server) register(c *command) {
	s.mu.Lock()
	s.commands[c.id] = c
	s.mu.Unlock()
}

func (s *Server) lookup(id string) *command {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.commands[id]
}

func (s *Server) runCommand(w http.ResponseWriter, r *http.Request) {
	req, err := decodeRunCommand(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid command request: "+err.Error())
		return
	}

	cmd, err := buildCmd(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid command request: "+err.Error())
		return
	}

	if req.Background {
		s.runBackground(w, req, cmd)
		return
	}

	s.runForeground(w, r, req, cmd)
}

// runForeground streams a command's output as it happens and ends the stream when it exits.
// The process dies with the request: a client that disconnects has nobody left to read the
// output, and upstream kills the group for the same reason.
func (s *Server) runForeground(w http.ResponseWriter, r *http.Request, req *runCommandRequest, cmd *exec.Cmd) {
	st := newStream(w)
	defer st.stop()

	id := newID()

	outR, outW, err := os.Pipe()
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeRuntimeError, "create stdout pipe: "+err.Error())
		return
	}

	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()

		writeError(w, http.StatusInternalServerError, codeRuntimeError, "create stderr pipe: "+err.Error())

		return
	}

	defer outR.Close()
	defer errR.Close()

	cmd.Stdout, cmd.Stderr = outW, errW

	started := time.Now()
	p, err := s.procs.start(cmd)

	// The child has its own copies of the write ends. Ours must go, or the pipes never reach
	// EOF.
	_ = outW.Close()
	_ = errW.Close()

	if err != nil {
		// Upstream answers a start failure inside the stream: init, then the error. The status
		// is 200 and the reason is where a client already looks for one.
		err = startHint(err, cmd)
		st.send(event{Type: evInit, Text: id})
		st.send(errorEvent("CommandExecError", err.Error(), err.Error()))

		return
	}

	c := &command{id: id, content: req.content(), startedAt: started, pgid: p.pid, running: true}
	s.register(c)

	st.send(event{Type: evInit, Text: id})
	st.startPing()

	var readers sync.WaitGroup

	for _, pipe := range []struct {
		f    *os.File
		kind string
	}{{outR, evStdout}, {errR, evStderr}} {
		readers.Add(1)

		go func() {
			defer readers.Done()
			pump(pipe.f, func(line string) { st.send(event{Type: pipe.kind, Text: line}) })
		}()
	}

	var timeout <-chan time.Time

	if req.Timeout > 0 {
		t := time.NewTimer(time.Duration(req.Timeout) * time.Millisecond)
		defer t.Stop()

		timeout = t.C
	}

	var why string

	select {
	case <-p.done:
	case <-r.Context().Done():
		why = "client disconnected"
		_ = signalGroup(p.pid, syscall.SIGKILL)
		<-p.done
	case <-timeout:
		why = fmt.Sprintf("timeout after %s", time.Duration(req.Timeout)*time.Millisecond)
		_ = signalGroup(p.pid, syscall.SIGKILL)
		<-p.done
	}

	deadline := time.Now().Add(drainGrace)
	_ = outR.SetReadDeadline(deadline)
	_ = errR.SetReadDeadline(deadline)
	readers.Wait()

	code, msg := p.exitCode(), p.describe()
	if why != "" {
		msg += " (" + why + ")"
	}

	if code == 0 {
		c.finish(0, "")
		st.send(event{Type: evComplete, ExecutionTime: time.Since(started).Milliseconds()})

		return
	}

	c.finish(code, msg)
	// evalue is the exit code as a string: the SDKs parse it back into Execution.ExitCode.
	st.send(errorEvent("CommandExecError", strconv.Itoa(code), msg))
}

// pump reads f until EOF or its read deadline, one event per line.
func pump(f *os.File, emit func(string)) {
	ls := &lineSplitter{emit: emit}
	buf := make([]byte, 32<<10)

	for {
		n, err := f.Read(buf)
		ls.write(buf[:n])

		if err != nil {
			break
		}
	}

	ls.flush()
}

// runBackground starts a detached command whose combined output goes to a file, and answers at
// once with init and execution_complete - upstream's shape, where "complete" means "started".
// Its output is polled through /command/{id}/logs and its end through /command/status/{id}.
func (s *Server) runBackground(w http.ResponseWriter, req *runCommandRequest, cmd *exec.Cmd) {
	id := newID()
	logPath := filepath.Join(s.outputDir, id+".output")

	out, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeRuntimeError,
			fmt.Sprintf("create output file for background command: %v; check that %s is writable", err, s.outputDir))

		return
	}

	devNull, err := os.Open(os.DevNull)
	if err == nil {
		defer devNull.Close()

		cmd.Stdin = devNull
	}

	cmd.Stdout, cmd.Stderr = out, out

	st := newStream(w)
	defer st.stop()

	started := time.Now()
	p, err := s.procs.start(cmd)
	_ = out.Close()

	c := &command{id: id, content: req.content(), background: true, logPath: logPath, startedAt: started}

	if err != nil {
		err = startHint(err, cmd)
		c.finish(255, err.Error())
		s.register(c)

		st.send(event{Type: evInit, Text: id})
		st.send(errorEvent("CommandExecError", err.Error(), err.Error()))

		return
	}

	c.pgid, c.running = p.pid, true
	s.register(c)

	go func() {
		var why string

		if req.Timeout > 0 {
			t := time.NewTimer(time.Duration(req.Timeout) * time.Millisecond)
			defer t.Stop()

			select {
			case <-p.done:
			case <-t.C:
				why = fmt.Sprintf(" (timeout after %s)", time.Duration(req.Timeout)*time.Millisecond)
				_ = signalGroup(p.pid, syscall.SIGKILL)
				<-p.done
			}
		}

		<-p.done

		if code := p.exitCode(); code != 0 {
			c.finish(code, p.describe()+why)
		} else {
			c.finish(0, "")
		}
	}()

	st.send(event{Type: evInit, Text: id})
	st.send(event{Type: evComplete, ExecutionTime: time.Since(started).Milliseconds()})
}

// interrupt is DELETE /command?id=, which upstream also accepts for a bash session id.
func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, codeMissingQuery, "missing query parameter 'id': the id from the command's init event")
		return
	}

	if c := s.lookup(id); c != nil {
		pgid := c.runningGroup()
		if pgid <= 0 {
			writeError(w, http.StatusInternalServerError, codeRuntimeError,
				fmt.Sprintf("command %s is not running; GET /command/status/%s has how it ended", id, id))

			return
		}

		if err := terminateGroup(pgid, interruptGrace); err != nil {
			writeError(w, http.StatusInternalServerError, codeRuntimeError, fmt.Sprintf("interrupt command %s: %v", id, err))
			return
		}

		w.WriteHeader(http.StatusOK)

		return
	}

	if s.closeSession(id) {
		w.WriteHeader(http.StatusOK)
		return
	}

	writeError(w, http.StatusNotFound, codeContextNotFound,
		fmt.Sprintf("no command or session %s; ids come from the init event of POST /command or from POST /session", id))
}

func (s *Server) commandSubresource(w http.ResponseWriter, r *http.Request) {
	a, b := r.PathValue("a"), r.PathValue("b")

	switch {
	case a == "status":
		s.commandStatus(w, b)
	case b == "logs":
		s.commandLogs(w, r, a)
	default:
		writeError(w, http.StatusNotFound, codeNotFound,
			fmt.Sprintf("%s is not an execd endpoint; use /command/status/{id} or /command/{id}/logs", r.URL.Path))
	}
}

// statusResponse is CommandStatusResponse. started_at is always present because the Go SDK
// decodes it into a non-pointer time.Time.
type statusResponse struct {
	ID         string     `json:"id"`
	Content    string     `json:"content,omitempty"`
	Running    bool       `json:"running"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func (s *Server) commandStatus(w http.ResponseWriter, id string) {
	c := s.lookup(id)
	if c == nil {
		writeError(w, http.StatusNotFound, codeInvalidRequest,
			fmt.Sprintf("command not found: %s; finished commands are kept for %s", id, commandRetention))

		return
	}

	c.mu.Lock()
	resp := statusResponse{
		ID: c.id, Content: c.content, Running: c.running, ExitCode: c.exitCode,
		Error: c.errMsg, StartedAt: c.startedAt, FinishedAt: c.finishedAt,
	}
	c.mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

// commandLogs is upstream's SeekBackgroundCommandOutput. The spec calls the cursor a line
// index; upstream has always treated it as a byte offset into the output, and the header it
// returns is the offset to pass next. Byte offsets are what clients have been built against,
// so that is what this is.
func (s *Server) commandLogs(w http.ResponseWriter, r *http.Request, id string) {
	c := s.lookup(id)
	if c == nil {
		writeError(w, http.StatusNotFound, codeInvalidRequest,
			fmt.Sprintf("command not found: %s; finished commands are kept for %s", id, commandRetention))

		return
	}

	if !c.background {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("command %s ran in the foreground; its output was in the SSE stream, only background commands keep logs", id))

		return
	}

	// An unparseable cursor reads from the start, as upstream's QueryInt64 does.
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	if cursor < 0 {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "cursor cannot be negative")
		return
	}

	f, err := os.Open(c.logPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeRuntimeError, fmt.Sprintf("open output of command %s: %v", id, err))
		return
	}
	defer f.Close()

	var data []byte

	if fi, err := f.Stat(); err == nil {
		cursor = min(cursor, fi.Size())

		if _, err := f.Seek(cursor, io.SeekStart); err == nil {
			// Bounded by the size seen now: a command still writing must not make this read
			// chase it forever, and the next poll picks up the rest from the returned cursor.
			var buf bytes.Buffer
			_, _ = io.CopyN(&buf, f, fi.Size()-cursor)
			data = buf.Bytes()
		}
	}

	w.Header().Set("EXECD-COMMANDS-TAIL-CURSOR", strconv.FormatInt(cursor+int64(len(data)), 10))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// janitor drops finished commands (and their output) older than retention, hourly, as upstream
// does. Running commands are never dropped.
func (s *Server) janitor(every, retention time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-s.stopJanitor:
			return
		case now := <-t.C:
			s.sweep(now.Add(-retention))
		}
	}
}

func (s *Server) sweep(cutoff time.Time) {
	var paths []string

	s.mu.Lock()
	for id, c := range s.commands {
		c.mu.Lock()
		old := !c.running && c.finishedAt != nil && c.finishedAt.Before(cutoff)
		c.mu.Unlock()

		if old {
			delete(s.commands, id)

			if c.logPath != "" {
				paths = append(paths, c.logPath)
			}
		}
	}
	s.mu.Unlock()

	for _, p := range paths {
		_ = os.Remove(p)
	}
}
