//go:build unix

package execd

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aryanmehrotra/sbx/internal/wsserver"
)

// Interactive PTY sessions, as upstream's components/execd/PTY.md describes them - a protocol
// that is not in execd-api.yaml, so upstream's source at release-1.1.0 is the specification:
//
//	POST   /pty             {cwd?, command?}  -> 201 {session_id}; nothing starts yet
//	GET    /pty/{id}                          -> {session_id, running, output_offset}
//	DELETE /pty/{id}                          -> kills the shell, forgets the session
//	GET    /pty/{id}/ws  ?pty=0 ?since=N ?takeover=1 ?mode=viewer
//
// The first read/write ("holder") connection starts the shell. There is one holder at a time; a
// second gets 409 unless it asks to take over, which closes the first with 4001 TAKEN_OVER. Any
// number of read-only viewers can watch a running shell. Frames:
//
//	server -> holder  binary 0x01+stdout, 0x02+stderr (pipe mode only)
//	server -> viewer  binary 0x03 + 8-byte big-endian offset + bytes (also replay to a holder)
//	client -> server  binary 0x00+stdin; text JSON {"type":"stdin|signal|resize|ping",...}
//	server -> client  text JSON connected / pong / error / exit
//
// Byte for byte the same framing, so a terminal client written against upstream attaches here
// unchanged.

const (
	ptyBinStdin  byte = 0x00
	ptyBinStdout byte = 0x01
	ptyBinStderr byte = 0x02
	ptyBinReplay byte = 0x03
)

// WebSocket error codes, upstream's model/pty_ws.go.
const (
	wsErrStartFailed      = "START_FAILED"
	wsErrStdinWriteFailed = "STDIN_WRITE_FAILED"
	wsErrInvalidFrame     = "INVALID_FRAME"
	wsErrAlreadyConnected = "ALREADY_CONNECTED"
	wsErrTakenOver        = "TAKEN_OVER"
	wsErrReadOnly         = "READ_ONLY"
	wsErrViewerNotRunning = "VIEWER_REQUIRES_RUNNING_SESSION"
)

const wsCloseTakenOver = 4001

// Timings are upstream's.
var (
	ptyPingInterval   = 30 * time.Second
	ptyIdleLimit      = 60 * time.Second
	ptyWriteTimeout   = 10 * time.Second
	ptyTakeoverWait   = 5 * time.Second
	ptyViewerStrikes  = 5
	ptyDefaultCols    = 80
	ptyDefaultRows    = 24
	ptyOutputChunk    = 32 * 1024
	ptyOutputDrainCap = 300 * time.Millisecond
)

type ptyCreateRequest struct {
	Cwd     string `json:"cwd,omitempty"`
	Command string `json:"command,omitempty"`
	// Cols and Rows are an sbx extension: the size the terminal starts at, so a client that
	// knows its size does not see the first screen drawn at 80x24 and then redrawn.
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
}

type ptyClientFrame struct {
	Type   string `json:"type"`
	Data   string `json:"data,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Signal string `json:"signal,omitempty"`
}

type ptyServerFrame struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Role      string `json:"role,omitempty"`
	Data      string `json:"data,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
	Timestamp int64  `json:"timestamp,omitempty"`
}

// ptySession is one shell, which can be started, exit, and be started again by the next holder
// - keeping its replay buffer, so scrollback survives a restart as upstream's does.
type ptySession struct {
	id, cwd, command string
	cols, rows       uint16
	procs            *procs
	replay           *replayBuffer

	mu         sync.Mutex
	closing    bool
	pid        int // 0 when not running
	exitCode   int
	isPTY      bool
	master     *os.File       // PTY mode, while running
	stdin      io.WriteCloser // master in PTY mode, a pipe in pipe mode
	done       chan struct{}  // this lifetime's; closed after its output is all in replay
	outputDone chan struct{}

	wsLocked atomic.Bool

	evictMu  sync.Mutex
	evict    func()
	evictGen uint64

	// outMu makes "append to replay" and "hand to the holder" one step, so a holder attaching
	// between the two can neither miss a chunk nor get it twice.
	outMu   sync.Mutex
	stdoutW *io.PipeWriter
	stderrW *io.PipeWriter
}

func (s *Server) createPTY(w http.ResponseWriter, r *http.Request) {
	if !ptyAvailable(w) {
		return
	}

	var req ptyCreateRequest

	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err == nil && len(strings.TrimSpace(string(data))) > 0 {
		err = json.Unmarshal(data, &req)
	}

	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"invalid pty request: "+err.Error()+`; send {"cwd":"/dir","command":"optional"} or no body`)

		return
	}

	if req.Cols < 0 || req.Rows < 0 || req.Cols > 65535 || req.Rows > 65535 {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "cols and rows must be between 1 and 65535")
		return
	}

	cwd := ""

	if req.Cwd != "" {
		cwd, err = expandPath(req.Cwd, userEnv(nil))
		if err == nil {
			err = os.MkdirAll(cwd, 0o755)
		}

		if err != nil {
			writeError(w, http.StatusInternalServerError, codeRuntimeError,
				fmt.Sprintf("error creating pty session work directory %s: %v", req.Cwd, err))

			return
		}
	}

	ps := &ptySession{
		id: newID(), cwd: cwd, command: req.Command, procs: s.procs,
		cols: uint16(ptyDefaultCols), rows: uint16(ptyDefaultRows),
		replay: newReplayBuffer(replaySize), exitCode: -1,
	}

	if req.Cols > 0 {
		ps.cols = uint16(req.Cols)
	}

	if req.Rows > 0 {
		ps.rows = uint16(req.Rows)
	}

	s.mu.Lock()
	s.ptys[ps.id] = ps
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]string{"session_id": ps.id})
}

func ptyAvailable(w http.ResponseWriter) bool {
	if !ptySupported {
		writeError(w, http.StatusNotImplemented, codeNotSupported,
			"pty sessions are not supported on this platform; sbx execd provides them on Linux")
	}

	return ptySupported
}

func (s *Server) ptySession(w http.ResponseWriter, r *http.Request) *ptySession {
	if !ptyAvailable(w) {
		return nil
	}

	id := r.PathValue("sessionId")

	s.mu.Lock()
	ps := s.ptys[id]
	s.mu.Unlock()

	if ps == nil {
		writeError(w, http.StatusNotFound, codeContextNotFound,
			fmt.Sprintf("pty session %s not found; create one with POST /pty", id))
	}

	return ps
}

func (s *Server) getPTY(w http.ResponseWriter, r *http.Request) {
	ps := s.ptySession(w, r)
	if ps == nil {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": ps.id, "running": ps.running(), "output_offset": ps.replay.Total(),
	})
}

func (s *Server) deletePTY(w http.ResponseWriter, r *http.Request) {
	ps := s.ptySession(w, r)
	if ps == nil {
		return
	}

	s.mu.Lock()
	delete(s.ptys, ps.id)
	s.mu.Unlock()

	ps.close()
	w.WriteHeader(http.StatusOK)
}

func (ps *ptySession) running() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	return ps.pid != 0
}

func (ps *ptySession) mode() string {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.isPTY {
		return "pty"
	}

	return "pipe"
}

func (ps *ptySession) lifetime() (done, outputDone <-chan struct{}) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	return ps.done, ps.outputDone
}

// command builds the shell: bash without profile or rc files when the image has it, else sh,
// with -c for a session created with a command. Upstream's choice, flags included.
func (ps *ptySession) buildCmd(pty bool) *exec.Cmd {
	sh := shell()

	var args []string
	if sh == "bash" {
		args = append(args, "--noprofile", "--norc")
	}

	if ps.command != "" {
		args = append(args, "-c", ps.command)
	}

	env := userEnv(nil)

	// A terminal with no TERM makes every curses program fall back to dumb mode; images rarely
	// set it because docker sets it only for -t.
	if pty && env["TERM"] == "" {
		env["TERM"] = "xterm-256color"
	}

	cmd := exec.Command(sh, args...)
	cmd.Env = envList(env)
	cmd.Dir = ps.cwd

	return cmd
}

// start runs the shell in a pseudo-terminal, or with plain pipes for pipe mode.
func (ps *ptySession) start(pipe bool) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.closing {
		return errors.New("pty session is closing")
	}

	if ps.pid != 0 {
		return errors.New("pty session already started")
	}

	// Checked here because exec reports a missing Dir as the shell itself not existing, which
	// sends whoever reads it looking for the wrong problem.
	if ps.cwd != "" {
		if fi, err := os.Stat(ps.cwd); err != nil || !fi.IsDir() {
			return fmt.Errorf("working directory %s no longer exists; create it again or start a new session with another cwd", ps.cwd)
		}
	}

	var (
		p       *proc
		readers []*os.File
		err     error
	)

	if pipe {
		p, readers, err = ps.startPipe()
	} else {
		p, readers, err = ps.startPTY()
	}

	if err != nil {
		return err
	}

	ps.isPTY = !pipe
	ps.pid = p.pid
	ps.exitCode = -1
	ps.done = make(chan struct{})
	ps.outputDone = make(chan struct{})

	var wg sync.WaitGroup

	for i, rd := range readers {
		wg.Add(1)

		stdout := i == 0

		go func() {
			defer wg.Done()
			ps.broadcast(rd, stdout)
		}()
	}

	outputDone, done := ps.outputDone, ps.done

	go func() {
		wg.Wait()
		close(outputDone)
	}()

	go ps.wait(p, readers, outputDone, done)

	return nil
}

func (ps *ptySession) startPTY() (*proc, []*os.File, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, nil, err
	}

	if err := setWinsize(master, ps.cols, ps.rows); err != nil {
		master.Close()
		slave.Close()

		return nil, nil, fmt.Errorf("set terminal size: %w", err)
	}

	cmd := ps.buildCmd(true)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = ptySysProcAttr()

	p, err := ps.procs.start(cmd)

	// The child has its own copy. Keeping ours open would stop the master ever reading EOF.
	slave.Close()

	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	ps.master, ps.stdin = master, master

	return p, []*os.File{master}, nil
}

func (ps *ptySession) startPipe() (*proc, []*os.File, error) {
	var files []*os.File

	closeAll := func() {
		for _, f := range files {
			f.Close()
		}
	}

	var pipes [3][2]*os.File

	for i := range pipes {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("create pipe: %w", err)
		}

		pipes[i] = [2]*os.File{r, w}
		files = append(files, r, w)
	}

	cmd := ps.buildCmd(false)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pipes[0][0], pipes[1][1], pipes[2][1]
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	p, err := ps.procs.start(cmd)

	// The child's ends, which the child now holds its own copies of.
	pipes[0][0].Close()
	pipes[1][1].Close()
	pipes[2][1].Close()

	if err != nil {
		pipes[0][1].Close()
		pipes[1][0].Close()
		pipes[2][0].Close()

		return nil, nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	ps.master, ps.stdin = nil, pipes[0][1]

	return p, []*os.File{pipes[1][0], pipes[2][0]}, nil
}

func (ps *ptySession) broadcast(r *os.File, stdout bool) {
	buf := make([]byte, ptyOutputChunk)

	for {
		n, err := r.Read(buf)
		if n > 0 {
			ps.fanout(buf[:n], stdout)
		}

		if err != nil {
			return // EOF on a pipe, EIO on a pty master once the last slave fd closes
		}
	}
}

func (ps *ptySession) fanout(chunk []byte, stdout bool) {
	ps.outMu.Lock()
	ps.replay.write(chunk)

	w := ps.stdoutW
	if !stdout {
		w = ps.stderrW
	}
	ps.outMu.Unlock()

	// Outside the lock: a slow holder holds up the shell's output, as upstream's does, but not
	// the next holder's attach.
	if w != nil {
		_, _ = w.Write(chunk)
	}
}

// wait records the exit once the shell is gone and its output is in the replay buffer. A
// background job that inherited the terminal can keep the output open indefinitely, so after a
// short grace the output is cut rather than letting the exit frame wait for it forever.
func (ps *ptySession) wait(p *proc, readers []*os.File, outputDone, done chan struct{}) {
	<-p.done

	select {
	case <-outputDone:
	case <-time.After(ptyOutputDrainCap):
	}

	for _, r := range readers {
		r.Close()
	}

	<-outputDone

	ps.mu.Lock()
	ps.exitCode = p.exitCode()
	ps.pid = 0

	if ps.stdin != nil {
		ps.stdin.Close()
	}

	ps.master, ps.stdin = nil, nil
	ps.mu.Unlock()

	close(done)
}

// attach snapshots the replay from since and becomes the live output sink in one step. pipe
// says whether to split stderr off; it is passed rather than read from the session because a
// holder attaches before it starts the shell.
func (ps *ptySession) attach(since int64, pipe bool) (stdout, stderr io.Reader, detach func(), snap []byte, at int64) {
	outR, outW := io.Pipe()

	var errR *io.PipeReader

	var errW *io.PipeWriter

	if pipe {
		errR, errW = io.Pipe()
	}

	ps.outMu.Lock()
	snap, at = ps.replay.readFrom(since)
	ps.stdoutW = outW

	if pipe {
		ps.stderrW = errW
	}
	ps.outMu.Unlock()

	detach = sync.OnceFunc(func() {
		ps.outMu.Lock()
		if ps.stdoutW == outW {
			ps.stdoutW = nil
		}

		if errW != nil && ps.stderrW == errW {
			ps.stderrW = nil
		}
		ps.outMu.Unlock()

		outW.Close()

		if errW != nil {
			errW.Close()
		}
	})

	if errR == nil {
		return outR, nil, detach, snap, at
	}

	return outR, errR, detach, snap, at
}

func (ps *ptySession) writeStdin(p []byte) error {
	ps.mu.Lock()
	w := ps.stdin
	ps.mu.Unlock()

	if w == nil {
		return errors.New("the shell is not running")
	}

	_, err := w.Write(p)

	return err
}

func (ps *ptySession) signal(name string) {
	sig, ok := map[string]syscall.Signal{
		"SIGINT": syscall.SIGINT, "SIGTERM": syscall.SIGTERM, "SIGKILL": syscall.SIGKILL,
		"SIGQUIT": syscall.SIGQUIT, "SIGHUP": syscall.SIGHUP,
	}[name]

	ps.mu.Lock()
	pid := ps.pid
	ps.mu.Unlock()

	if ok && pid != 0 {
		// The shell leads its own process group in both modes, so this reaches its jobs too.
		_ = syscall.Kill(-pid, sig)
	}
}

func (ps *ptySession) resize(cols, rows int) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if cols < 1 || rows < 1 || cols > 65535 || rows > 65535 {
		return nil
	}

	ps.cols, ps.rows = uint16(cols), uint16(rows)

	if ps.master == nil {
		return nil // pipe mode, or not running: the size applies at the next start
	}

	return setWinsize(ps.master, ps.cols, ps.rows)
}

func (ps *ptySession) close() {
	ps.mu.Lock()
	if ps.closing {
		ps.mu.Unlock()
		return
	}

	ps.closing = true
	pid, stdin := ps.pid, ps.stdin
	ps.mu.Unlock()

	if pid != 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}

	if stdin != nil {
		stdin.Close()
	}

	ps.outMu.Lock()
	outW, errW := ps.stdoutW, ps.stderrW
	ps.stdoutW, ps.stderrW = nil, nil
	ps.outMu.Unlock()

	if outW != nil {
		outW.Close()
	}

	if errW != nil {
		errW.Close()
	}

	ps.triggerEvict()
}

func (ps *ptySession) setEvict(fn func()) uint64 {
	ps.evictMu.Lock()
	defer ps.evictMu.Unlock()

	ps.evictGen++
	ps.evict = fn

	return ps.evictGen
}

// clearEvict removes the hook only if it is still gen's, so a handler tearing down after a
// takeover never removes its successor's.
func (ps *ptySession) clearEvict(gen uint64) {
	ps.evictMu.Lock()
	defer ps.evictMu.Unlock()

	if ps.evictGen == gen {
		ps.evict = nil
	}
}

func (ps *ptySession) triggerEvict() {
	ps.evictMu.Lock()
	fn := ps.evict
	ps.evictMu.Unlock()

	if fn != nil {
		fn()
	}
}

// takeover evicts the holder until the lock is free, or gives up after wait.
func (ps *ptySession) takeover(wait time.Duration) bool {
	deadline := time.Now().Add(wait)

	for {
		if ps.wsLocked.CompareAndSwap(false, true) {
			return true
		}

		ps.triggerEvict()

		if time.Now().After(deadline) {
			return ps.wsLocked.CompareAndSwap(false, true)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// ptyConn is what the holder and viewer handlers share: a connection, a way to stop everything
// attached to it, and a record of when the client was last heard from.
type ptyConn struct {
	c        *wsserver.Conn
	cancel   func()
	stopped  chan struct{}
	lastRead atomic.Int64
	wg       sync.WaitGroup
}

func newPTYConn(c *wsserver.Conn) *ptyConn {
	pc := &ptyConn{c: c, stopped: make(chan struct{})}
	pc.cancel = sync.OnceFunc(func() {
		close(pc.stopped)
		_ = c.Close()
	})
	pc.lastRead.Store(time.Now().UnixNano())

	return pc
}

func (pc *ptyConn) sendJSON(f ptyServerFrame) error {
	b, _ := json.Marshal(f)

	return pc.c.WriteMessage(wsserver.TextMessage, b)
}

func (pc *ptyConn) sendReplay(data []byte, offset int64) error {
	frame := make([]byte, 9+len(data))
	frame[0] = ptyBinReplay
	binary.BigEndian.PutUint64(frame[1:9], uint64(offset))
	copy(frame[9:], data)

	return pc.c.WriteMessage(wsserver.BinaryMessage, frame)
}

// go runs fn as one of the connection's goroutines, which the handler waits for.
func (pc *ptyConn) goRun(fn func()) {
	pc.wg.Add(1)

	go func() {
		defer pc.wg.Done()
		fn()
	}()
}

// keepalive pings, and ends the connection when the client has been silent - neither a frame
// nor a pong - for ptyIdleLimit: a client behind a proxy that dropped it without a FIN would
// otherwise hold the session's one holder slot forever.
func (pc *ptyConn) keepalive() {
	t := time.NewTicker(ptyPingInterval)
	defer t.Stop()

	for {
		select {
		case <-pc.stopped:
			return
		case <-t.C:
			last := time.Unix(0, pc.lastRead.Load())
			if p := pc.c.LastPong(); p.After(last) {
				last = p
			}

			if time.Since(last) > ptyIdleLimit {
				pc.cancel()
				return
			}

			if err := pc.c.Ping(); err != nil {
				pc.cancel()
				return
			}
		}
	}
}

// exitWhen sends the exit frame once done closes and ready (the output flushed to this client)
// has returned, then closes the connection normally.
func (ps *ptySession) exitWhen(pc *ptyConn, done <-chan struct{}, ready func() bool) {
	select {
	case <-done:
	case <-pc.stopped:
		return
	}

	if !ready() {
		return
	}

	ps.mu.Lock()
	code := ps.exitCode
	ps.mu.Unlock()

	_ = pc.sendJSON(ptyServerFrame{Type: "exit", ExitCode: &code})
	_ = pc.c.CloseWith(wsserver.CloseNormal, "process exited")
	pc.cancel()
}

func (s *Server) ptyWebSocket(w http.ResponseWriter, r *http.Request) {
	ps := s.ptySession(w, r)
	if ps == nil {
		return
	}

	q := r.URL.Query()

	if q.Get("mode") == "viewer" {
		s.ptyViewer(w, r, ps)
		return
	}

	// Refused with an HTTP 409 before the upgrade, so a client sees why without speaking
	// WebSocket. A takeover evicts only after its own handshake succeeded: a request that
	// announces an upgrade and then fails it must not kill the holder it meant to replace.
	locked := ps.wsLocked.CompareAndSwap(false, true)
	if !locked && (q.Get("takeover") != "1" || !wsserver.IsUpgrade(r)) {
		writeError(w, http.StatusConflict, wsErrAlreadyConnected,
			"another client is already connected to pty session "+ps.id+"; add ?takeover=1 to replace it or ?mode=viewer to watch")

		return
	}

	c, err := wsserver.Upgrade(w, r, wsserver.Options{WriteTimeout: ptyWriteTimeout})
	if err != nil {
		if locked {
			ps.wsLocked.Store(false)
		}

		return
	}

	pc := newPTYConn(c)

	if !locked && !ps.takeover(ptyTakeoverWait) {
		_ = pc.sendJSON(ptyServerFrame{Type: "error", Code: wsErrAlreadyConnected, Error: "takeover timed out for pty session " + ps.id})
		pc.cancel()

		return
	}

	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)

	// Attached before the shell starts, so its first output reaches this holder live - split
	// into stdout and stderr in pipe mode - rather than only inside a combined replay frame.
	starting := !ps.running()

	pipe := ps.mode() == "pipe"
	if starting {
		pipe = q.Get("pty") == "0"
	}

	stdout, stderr, detach, snap, at := ps.attach(since, pipe)

	if starting {
		if err := ps.start(pipe); err != nil {
			s.log.Printf("pty %s: start: %v", ps.id, err)
			detach()
			_ = pc.sendJSON(ptyServerFrame{Type: "error", Code: wsErrStartFailed, Error: err.Error()})
			pc.cancel()
			ps.wsLocked.Store(false)

			return
		}
	}

	var pumps sync.WaitGroup

	var gen uint64

	defer func() {
		pc.cancel()
		detach()
		pumps.Wait()
		pc.wg.Wait()
		ps.wsLocked.Store(false)
		ps.clearEvict(gen)
	}()

	// Set before the first write, so a takeover can interrupt a holder stalled mid-replay.
	// CloseNow never waits behind a stalled writer: it closes the socket, which unsticks it.
	gen = ps.setEvict(func() {
		pc.c.CloseNow(wsCloseTakenOver, wsErrTakenOver)
		pc.cancel()
	})

	if len(snap) > 0 {
		if err := pc.sendReplay(snap, at); err != nil {
			return
		}
	}

	if err := pc.sendJSON(ptyServerFrame{Type: "connected", SessionID: ps.id, Mode: ps.mode(), Role: "holder"}); err != nil {
		return
	}

	pump := func(r io.Reader, tag byte) {
		defer pumps.Done()

		buf := make([]byte, 1+ptyOutputChunk)
		buf[0] = tag

		for {
			n, err := r.Read(buf[1:])
			if n > 0 {
				if werr := pc.c.WriteMessage(wsserver.BinaryMessage, buf[:1+n]); werr != nil {
					pc.cancel()
					return
				}
			}

			if err != nil {
				return
			}
		}
	}

	pumps.Add(1)

	go pump(stdout, ptyBinStdout)

	if stderr != nil {
		pumps.Add(1)

		go pump(stderr, ptyBinStderr)
	}

	done, _ := ps.lifetime()

	pc.goRun(pc.keepalive)
	pc.goRun(func() {
		ps.exitWhen(pc, done, func() bool {
			// Everything the shell wrote is in the pipes by now; closing them lets the pumps
			// finish what they hold and stop, so the exit frame comes after the last output.
			detach()
			pumps.Wait()

			return true
		})
	})

	ps.holderReads(pc)
}

func (ps *ptySession) holderReads(pc *ptyConn) {
	for {
		typ, data, err := pc.c.ReadMessage()
		if err != nil {
			pc.cancel()
			return
		}

		pc.lastRead.Store(time.Now().UnixNano())

		switch typ {
		case wsserver.BinaryMessage:
			if len(data) == 0 || data[0] != ptyBinStdin {
				continue // only stdin travels client to server in binary
			}

			if err := ps.writeStdin(data[1:]); err != nil {
				_ = pc.sendJSON(ptyServerFrame{Type: "error", Code: wsErrStdinWriteFailed, Error: err.Error()})
				pc.cancel()

				return
			}
		case wsserver.TextMessage:
			var f ptyClientFrame
			if json.Unmarshal(data, &f) != nil {
				continue
			}

			switch f.Type {
			case "stdin":
				if err := ps.writeStdin([]byte(f.Data)); err != nil {
					_ = pc.sendJSON(ptyServerFrame{Type: "error", Code: wsErrStdinWriteFailed, Error: err.Error()})
					pc.cancel()

					return
				}
			case "signal":
				ps.signal(f.Signal)
			case "resize":
				_ = ps.resize(f.Cols, f.Rows)
			case "ping":
				_ = pc.sendJSON(ptyServerFrame{Type: "pong"})
			default:
				_ = pc.sendJSON(ptyServerFrame{Type: "error", Code: wsErrInvalidFrame, Error: fmt.Sprintf("unknown frame type %q", f.Type)})
			}
		}
	}
}

// ptyViewer serves a read-only attachment. Viewers read the replay buffer rather than a live
// pipe, so a slow viewer can fall behind - and see from the replay frame's offset that it did -
// without ever holding up the holder or the shell.
func (s *Server) ptyViewer(w http.ResponseWriter, r *http.Request, ps *ptySession) {
	// A viewer cannot start the shell: the holder that would drive it would not exist.
	if !ps.running() {
		writeError(w, http.StatusConflict, wsErrViewerNotRunning,
			"pty session "+ps.id+" must be running before a viewer can attach; connect a read/write client first")

		return
	}

	c, err := wsserver.Upgrade(w, r, wsserver.Options{WriteTimeout: ptyWriteTimeout})
	if err != nil {
		return
	}

	pc := newPTYConn(c)
	defer func() {
		pc.cancel()
		pc.wg.Wait()
	}()

	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	done, outputDone := ps.lifetime()

	snap, at, changed := ps.replay.readAndSubscribe(since)
	next := at + int64(len(snap))

	if len(snap) > 0 {
		if err := pc.sendReplay(snap, at); err != nil {
			return
		}
	}

	if err := pc.sendJSON(ptyServerFrame{Type: "connected", SessionID: ps.id, Mode: ps.mode(), Role: "viewer"}); err != nil {
		return
	}

	drained := make(chan struct{})

	pc.goRun(pc.keepalive)
	pc.goRun(func() {
		drain := func() bool {
			var data []byte

			var at int64

			data, at, changed = ps.replay.readAndSubscribe(next)
			next = at + int64(len(data))

			if len(data) == 0 {
				return true
			}

			if err := pc.sendReplay(data, at); err != nil {
				pc.cancel()
				return false
			}

			return true
		}

		for {
			select {
			case <-pc.stopped:
				return
			case <-changed:
				if !drain() {
					return
				}
			case <-outputDone:
				if drain() {
					close(drained)
				}

				return
			}
		}
	})
	pc.goRun(func() {
		ps.exitWhen(pc, done, func() bool {
			select {
			case <-drained:
				return true
			case <-pc.stopped:
				return false
			}
		})
	})

	strikes := 0

	for {
		typ, data, err := pc.c.ReadMessage()
		if err != nil {
			return
		}

		pc.lastRead.Store(time.Now().UnixNano())

		mutating := typ == wsserver.BinaryMessage && len(data) > 0 && data[0] == ptyBinStdin

		if typ == wsserver.TextMessage {
			var f ptyClientFrame
			if json.Unmarshal(data, &f) != nil {
				continue
			}

			switch f.Type {
			case "stdin", "signal", "resize":
				mutating = true
			case "ping":
				_ = pc.sendJSON(ptyServerFrame{Type: "pong"})
			default:
				_ = pc.sendJSON(ptyServerFrame{Type: "error", Code: wsErrInvalidFrame, Error: fmt.Sprintf("unknown frame type %q", f.Type)})
			}
		}

		if mutating {
			strikes++

			if pc.sendJSON(ptyServerFrame{Type: "error", Code: wsErrReadOnly, Error: "viewer connections are read-only"}) != nil ||
				strikes >= ptyViewerStrikes {
				return
			}
		}
	}
}
