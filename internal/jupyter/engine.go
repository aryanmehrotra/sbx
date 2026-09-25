package jupyter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/wsclient"
)

// Context is a code execution context: one Jupyter session and the kernel behind it. It
// marshals as execd-api.yaml's CodeContext.
type Context struct {
	ID       string `json:"id"`
	Language string `json:"language"`
}

// kernelCtx is the registry entry for one context.
type kernelCtx struct {
	Context

	kernelID string
	created  time.Time

	// run is held for the whole of an execution. TryLock, not Lock: upstream refuses a second
	// execution on a busy context instead of queueing it.
	run sync.Mutex
}

// Engine runs code in Jupyter kernels. It is safe for concurrent use; contexts are independent
// of one another and may execute in parallel.
type Engine struct {
	cfg  Config
	rest *rest

	mu       sync.Mutex
	contexts map[string]*kernelCtx
	defaults map[string]string      // language -> context id of the implicit context
	creating map[string]*sync.Mutex // language -> serialises creating the implicit context
}

// New returns an engine for a Jupyter server. It does not contact the server; Available does.
// A config with no BaseURL is accepted, so execd can always construct one and ask it why code
// execution is unavailable.
func New(cfg Config) *Engine {
	cfg = cfg.withDefaults()

	e := &Engine{
		cfg:      cfg,
		contexts: map[string]*kernelCtx{},
		defaults: map[string]string{},
		creating: map[string]*sync.Mutex{},
	}

	if u, err := cfg.validate(); err == nil {
		e.rest = &rest{base: u, token: cfg.Token, client: cfg.HTTPClient}
	}

	return e
}

func (e *Engine) client() (*rest, error) {
	if e.rest == nil {
		_, err := e.cfg.validate()

		return nil, err
	}

	return e.rest, nil
}

// Available reports whether code can be executed here, and if not, why. nil means a Jupyter
// server answered with at least one kernel. The error wraps ErrNotConfigured when the image has
// no Jupyter at all and ErrUnavailable when it has one that is not answering; either way its
// text is fit to return to the caller as the reason for a 501.
func (e *Engine) Available(ctx context.Context) error {
	r, err := e.client()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	ks, err := r.kernelSpecs(ctx)
	if err != nil {
		return fmt.Errorf("%w at %s: %v", ErrUnavailable, e.cfg.BaseURL, err)
	}

	if len(ks.Kernelspecs) == 0 {
		return fmt.Errorf("%w: the Jupyter server at %s has no kernels installed", ErrUnavailable, e.cfg.BaseURL)
	}

	return nil
}

// CreateContext starts a kernel for a language and returns its context. cwd, when set, is the
// kernel's working directory and is created if missing. It retries for Config.StartupWait while
// the Jupyter server is not answering yet.
func (e *Engine) CreateContext(ctx context.Context, language, cwd string) (Context, error) {
	kc, err := e.create(ctx, language, cwd)
	if err != nil {
		return Context{}, err
	}

	return kc.Context, nil
}

func (e *Engine) create(ctx context.Context, language, cwd string) (*kernelCtx, error) {
	lang := normalizeLanguage(language)
	if !IsKernelLanguage(lang) {
		return nil, fmt.Errorf("%w: %q is not run through Jupyter (supported: %s)", ErrUnsupportedLanguage, language, strings.Join(Languages, ", "))
	}

	r, err := e.client()
	if err != nil {
		return nil, err
	}

	name := newID()

	nbPath, err := notebookPath(name, cwd)
	if err != nil {
		return nil, err
	}

	var s *sessionResp

	err = e.retry(ctx, func() error {
		ks, err := r.kernelSpecs(ctx)
		if err != nil {
			return err
		}

		kernel, err := ks.pickKernel(lang)
		if err != nil {
			return err
		}

		s, err = r.createSession(ctx, name, nbPath, kernel)

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("create %s context: %w", lang, err)
	}

	kc := &kernelCtx{
		Context:  Context{ID: s.ID, Language: lang},
		kernelID: s.Kernel.ID,
		created:  time.Now(),
	}

	e.mu.Lock()
	e.contexts[kc.ID] = kc
	e.mu.Unlock()

	return kc, nil
}

// notebookPath is where the session's notebook nominally lives. Nothing is ever written there;
// Jupyter uses the directory as the kernel's cwd. With no cwd the path is relative, which puts
// the kernel in the server's root directory - upstream's behaviour. Jupyter resolves the path
// under its root, so a cwd must be relative to that root or inside it; an absolute path outside
// the root falls back to the root, in upstream too (checked against opensandbox/code-interpreter).
func notebookPath(name, cwd string) (string, error) {
	if cwd == "" {
		return name + ".ipynb", nil
	}

	if cwd == "~" || strings.HasPrefix(cwd, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand %q: %w", cwd, err)
		}

		cwd = filepath.Join(home, strings.TrimPrefix(cwd, "~"))
	}

	cwd = os.ExpandEnv(cwd)

	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return "", fmt.Errorf("create working directory %s for the context: %w", cwd, err)
	}

	return filepath.Join(cwd, name+".ipynb"), nil
}

// retry runs fn until it succeeds, fails permanently, or StartupWait runs out.
func (e *Engine) retry(ctx context.Context, fn func() error) error {
	deadline := time.Now().Add(e.cfg.StartupWait)
	wait := 200 * time.Millisecond

	for {
		err := fn()
		if err == nil || !transient(err) || ctx.Err() != nil {
			return err
		}

		if time.Now().Add(wait).After(deadline) {
			return fmt.Errorf("%w: still not answering after %s (last error: %v) - is Jupyter Server running at %s?",
				ErrUnavailable, e.cfg.StartupWait, err, e.cfg.BaseURL)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}

		wait = min(wait*3/2, 3*time.Second)
	}
}

// GetContext returns a context by id.
func (e *Engine) GetContext(id string) (Context, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	kc, ok := e.contexts[id]
	if !ok {
		return Context{}, fmt.Errorf("%w: %s", ErrContextNotFound, id)
	}

	return kc.Context, nil
}

// ListContexts returns the contexts for a language, or all of them for "", oldest first. The
// implicit per-language contexts that Run creates are included, as upstream includes them.
func (e *Engine) ListContexts(language string) []Context {
	lang := normalizeLanguage(language)

	e.mu.Lock()

	kcs := make([]*kernelCtx, 0, len(e.contexts))
	for _, kc := range e.contexts {
		if lang == "" || kc.Language == lang {
			kcs = append(kcs, kc)
		}
	}

	e.mu.Unlock()

	sort.Slice(kcs, func(i, j int) bool {
		if kcs[i].created.Equal(kcs[j].created) {
			return kcs[i].ID < kcs[j].ID
		}

		return kcs[i].created.Before(kcs[j].created)
	})

	out := make([]Context, len(kcs))
	for i, kc := range kcs {
		out[i] = kc.Context
	}

	return out
}

// DeleteContext shuts a context's kernel down and forgets it. An execution running in it ends
// with an error event, because its kernel goes away underneath it.
func (e *Engine) DeleteContext(ctx context.Context, id string) error {
	e.mu.Lock()
	_, ok := e.contexts[id]
	e.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrContextNotFound, id)
	}

	r, err := e.client()
	if err != nil {
		return err
	}

	if err := r.deleteSession(ctx, id); err != nil {
		return fmt.Errorf("delete context %s: %w", id, err)
	}

	e.forget(id)

	return nil
}

func (e *Engine) forget(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if kc, ok := e.contexts[id]; ok && e.defaults[kc.Language] == id {
		delete(e.defaults, kc.Language)
	}

	delete(e.contexts, id)
}

// DeleteContextsByLanguage deletes every context of a language, the implicit one included. It
// carries on past a failure so one stuck kernel does not keep the rest alive, and reports every
// failure.
func (e *Engine) DeleteContextsByLanguage(ctx context.Context, language string) error {
	var errs []error

	for _, c := range e.ListContexts(language) {
		if err := e.DeleteContext(ctx, c.ID); err != nil && !errors.Is(err, ErrContextNotFound) {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Interrupt interrupts whatever the context's kernel is running. The running execution ends with
// the kernel's own error (KeyboardInterrupt in Python) followed by execution_complete.
func (e *Engine) Interrupt(ctx context.Context, id string) error {
	e.mu.Lock()
	kc, ok := e.contexts[id]
	e.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrContextNotFound, id)
	}

	r, err := e.client()
	if err != nil {
		return err
	}

	if err := r.interrupt(ctx, kc.kernelID); err != nil {
		return fmt.Errorf("interrupt context %s: %w", id, err)
	}

	return nil
}

// RunRequest is one execution. With ContextID empty, the code runs in the language's implicit
// context, which is created on first use and then kept - so bare runs share state, as upstream's
// do.
type RunRequest struct {
	ContextID string
	Language  string
	Code      string
}

// Run executes code and reports it through emit, synchronously: Run returns once the stream is
// over, and emit is never called after that or from two goroutines at once.
//
// Errors come in two kinds, told apart by whether emit has been called:
//   - Before the first emit - ErrContextNotFound, ErrContextBusy, ErrUnsupportedLanguage,
//     ErrNotConfigured, ErrUnavailable, a failed websocket connect - nothing has been streamed
//     and the caller should answer with an ordinary error response.
//   - After it, the only error returned is the context's, when the caller cancelled. Everything
//     that goes wrong in the kernel - an exception, an interrupt, the kernel dying - is an error
//     event in the stream, and Run returns nil.
//
// The stream is init, then any of stdout/stderr/execution_count/result/ping, then either
// execution_complete, or status("error") + error + execution_complete for an exception - that
// status event is upstream's and kept for fidelity - or a lone error event when the kernel died
// or the caller cancelled.
func (e *Engine) Run(ctx context.Context, req RunRequest, emit func(Event)) error {
	kc, err := e.resolve(ctx, req)
	if err != nil {
		return err
	}

	if !kc.run.TryLock() {
		return fmt.Errorf("%w: context %s is already executing - wait for it or interrupt it", ErrContextBusy, kc.ID)
	}
	defer kc.run.Unlock()

	r, err := e.client()
	if err != nil {
		return err
	}

	// A fresh websocket per execution, as upstream does. A shared one would need its reader to
	// route every message to whichever execution is current, and a message arriving late from
	// the previous cell would have somewhere to go wrong; a per-run socket closes that off.
	var hdr http.Header
	if r.token != "" {
		hdr = http.Header{"Authorization": {"token " + r.token}}
	}

	dial := func() (*kernelSocket, error) {
		dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
		defer cancelDial()

		return dialKernel(dialCtx, r.channelsURL, kc.kernelID, hdr)
	}

	ks, err := dial()
	if err != nil {
		return fmt.Errorf("%w: connect to kernel %s of context %s: %v", ErrUnavailable, kc.kernelID, kc.ID, err)
	}

	defer func() {
		if ks != nil {
			ks.close()
		}
	}()

	send := func(ev Event) {
		ev.Timestamp = time.Now().UnixMilli()
		emit(ev)
	}

	lost := func(what string, err error) error {
		send(Event{Type: EventError, Error: &ErrorOutput{
			EName:  "KernelConnectionLost",
			EValue: fmt.Sprintf("%s: %v - retry, or recreate the context", what, err),
		}})

		return nil
	}

	send(Event{Type: EventInit, Text: kc.ID})

	// The kernel must answer on this socket before the cell is sent; a socket it never answers on
	// is replaced rather than retried, because that is the failure seen in practice (see
	// awaitKernel). The whole wait is bounded by kernelAnswerWait.
	budget := time.Now().Add(kernelAnswerWait)

	for {
		err := awaitKernel(ctx, ks)
		if err == nil {
			break
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if time.Now().After(budget) {
			return lost(fmt.Sprintf("the kernel did not answer within %s", kernelAnswerWait), err)
		}

		ks.close()

		if ks, err = dial(); err != nil {
			return lost("could not reconnect to the kernel", err)
		}
	}

	msg := newExecuteRequest(ks.session, req.Code)

	payload, err := json.Marshal(msg)
	if err != nil {
		return err // unreachable: every field is a plain value
	}

	start := time.Now()

	if err := ks.conn.WriteText(payload); err != nil {
		return lost("could not send the code to the kernel", err)
	}

	x := execution{id: msg.Header.MsgID, send: send, start: start}

	return x.pump(ctx, e, kc, ks.msgs, ks.readErr)
}

// resolve finds the context a request names, creating the implicit one if it names none.
func (e *Engine) resolve(ctx context.Context, req RunRequest) (*kernelCtx, error) {
	if req.ContextID != "" {
		e.mu.Lock()
		kc, ok := e.contexts[req.ContextID]
		e.mu.Unlock()

		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrContextNotFound, req.ContextID)
		}

		return kc, nil
	}

	lang := normalizeLanguage(req.Language)
	if !IsKernelLanguage(lang) {
		return nil, fmt.Errorf("%w: %q is not run through Jupyter (supported: %s)", ErrUnsupportedLanguage, req.Language, strings.Join(Languages, ", "))
	}

	// One creator per language: two first requests arriving together must share one implicit
	// context, not start two kernels and orphan whichever loses.
	e.mu.Lock()

	m, ok := e.creating[lang]
	if !ok {
		m = &sync.Mutex{}
		e.creating[lang] = m
	}

	e.mu.Unlock()

	m.Lock()
	defer m.Unlock()

	e.mu.Lock()
	if id, ok := e.defaults[lang]; ok {
		if kc, ok := e.contexts[id]; ok {
			e.mu.Unlock()

			return kc, nil
		}
	}
	e.mu.Unlock()

	kc, err := e.create(ctx, lang, "")
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	e.defaults[lang] = kc.ID
	e.mu.Unlock()

	return kc, nil
}

func readLoop(conn *wsclient.Conn, out chan<- inbound, errc chan<- error, done <-chan struct{}) {
	for {
		typ, p, err := conn.ReadMessage()
		if err != nil {
			errc <- err

			return
		}

		// Binary frames carry messages with buffers (widgets, comms). Nothing in a code
		// execution's output needs them.
		if typ != wsclient.TextMessage {
			continue
		}

		var m inbound
		if json.Unmarshal(p, &m) != nil {
			continue
		}

		select {
		case out <- m:
		case <-done:
			return
		}
	}
}

// execution is the state of one running cell.
type execution struct {
	id    string
	send  func(Event)
	start time.Time

	idle     bool
	replied  bool
	sawError bool
	reply    executeReplyContent
}

func (x *execution) pump(ctx context.Context, e *Engine, kc *kernelCtx, msgs <-chan inbound, readErr <-chan error) error {
	ping := time.NewTicker(e.cfg.PingInterval)
	defer ping.Stop()

	// grace starts once the kernel is idle: the execute_reply travels on a different channel
	// from the idle status, so either can arrive first, and a kernel that never sends one must
	// not hold the stream open forever - which upstream's does.
	var grace <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			// The caller went away or gave up. Leaving the kernel running would keep the context
			// busy for a result nobody will read, so interrupt it, as upstream does.
			ictx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = e.Interrupt(ictx, kc.ID)

			cancel()
			x.send(Event{Type: EventError, Error: &ErrorOutput{EName: "ContextCancelled", EValue: "Interrupt kernel"}})

			return ctx.Err()

		case <-ping.C:
			x.send(Event{Type: EventPing, Text: "pong"})

		case <-grace:
			// No reply, but the kernel is idle, so the cell has finished. End the stream the way
			// a normal one ends rather than leaving the caller without a terminal event.
			x.complete()

			return nil

		case err := <-readErr:
			// The reader queues every message before it reports the error, but select picks
			// among ready cases at random - so the last output before a crash can still be
			// sitting in msgs. It is the output that explains the crash; deliver it first.
			for drained := false; !drained; {
				select {
				case m := <-msgs:
					if x.handle(m, kc) {
						return nil
					}
				default:
					drained = true
				}
			}

			if x.idle {
				// The output is complete; only the reply was outstanding.
				x.complete()

				return nil
			}

			x.send(Event{Type: EventError, Error: &ErrorOutput{
				EName: "KernelConnectionLost",
				EValue: fmt.Sprintf("the connection to the kernel closed mid-execution (%v) - the kernel may have died; "+
					"check the context with GET /code/contexts/%s and recreate it if it is gone", err, kc.ID),
			}})

			return nil

		case m := <-msgs:
			if done := x.handle(m, kc); done {
				return nil
			}

			if x.idle && grace == nil {
				grace = time.After(e.cfg.ReplyGrace)
			}
		}
	}
}

// handle applies one kernel message and reports whether the execution is over.
func (x *execution) handle(m inbound, kc *kernelCtx) bool {
	// Kernel death is announced by the server with no parent: the restarter, not the cell,
	// produced it. It has to be checked before the parent filter or it would be dropped.
	if m.Header.MsgType == "status" {
		var s statusContent
		if json.Unmarshal(m.Content, &s) == nil && (s.ExecutionState == "dead" || s.ExecutionState == "restarting") {
			x.send(Event{Type: EventError, Error: &ErrorOutput{
				EName: "KernelDied",
				EValue: fmt.Sprintf("the kernel of context %s exited mid-execution (execution_state=%s); "+
					"its variables are gone - re-run the setup cells, or delete and recreate the context", kc.ID, s.ExecutionState),
			}})

			return true
		}
	}

	// Everything else must be a reply to this cell. The server sends status traffic of its own
	// on connect (its kernel_info probe), and a background thread from an earlier cell can still
	// be printing - neither belongs in this stream.
	if m.ParentHeader.MsgID != x.id {
		return false
	}

	switch m.Header.MsgType {
	case "stream":
		var c streamContent
		if json.Unmarshal(m.Content, &c) != nil || c.Text == "" {
			return false
		}

		switch c.Name {
		case "stdout":
			x.send(Event{Type: EventStdout, Text: c.Text})
		case "stderr":
			x.send(Event{Type: EventStderr, Text: c.Text})
		}

	case "execute_result":
		var c executeResultContent
		if json.Unmarshal(m.Content, &c) != nil {
			return false
		}

		if c.ExecutionCount > 0 {
			x.send(Event{Type: EventExecutionCount, ExecutionCount: c.ExecutionCount})
		}

		if res := resultsFromData(c.Data); res != nil {
			x.send(Event{Type: EventResult, Results: res})
		}

	case "display_data":
		// Upstream drops display_data, so a plot never reaches the caller. It is sent here as a
		// result, which is what a client reading results expects a figure to be.
		var c displayDataContent
		if json.Unmarshal(m.Content, &c) != nil {
			return false
		}

		if res := resultsFromData(c.Data); res != nil {
			x.send(Event{Type: EventResult, Results: res})
		}

	case "error":
		var c ErrorOutput
		if json.Unmarshal(m.Content, &c) != nil {
			return false
		}

		x.sawError = true
		x.send(Event{Type: EventStatus, Text: "error"})
		x.send(Event{Type: EventError, Error: &c})

	case "status":
		var c statusContent
		if json.Unmarshal(m.Content, &c) != nil || c.ExecutionState != "idle" || x.idle {
			return false
		}

		x.idle = true

		return x.finishIfDone()

	case "execute_reply":
		if json.Unmarshal(m.Content, &x.reply) != nil {
			return false
		}

		x.replied = true

		return x.finishIfDone()
	}

	return false
}

// finishIfDone ends the stream once the kernel is idle and has replied.
//
// execution_complete follows an error too, as upstream's does. It is sent at idle-and-replied
// rather than at idle alone, which only moves it after the reply-only error below. An error
// reported only in the reply - no iopub error message at all - is surfaced here rather than
// lost; it can only be judged at this point, because before idle the iopub error may simply not
// have arrived yet.
func (x *execution) finishIfDone() bool {
	if !x.idle || !x.replied {
		return false
	}

	if x.reply.Status == "error" && !x.sawError {
		x.send(Event{Type: EventStatus, Text: "error"})
		x.send(Event{Type: EventError, Error: &ErrorOutput{EName: x.reply.EName, EValue: x.reply.EValue, Traceback: x.reply.Traceback}})
	}

	x.complete()

	return true
}

func (x *execution) complete() {
	x.send(Event{Type: EventComplete, ExecutionTime: time.Since(x.start).Milliseconds()})
}

// Handshake timings. kernelAnswerWait bounds the whole wait before a cell is sent; a socket gets
// kernelSocketWait to answer before it is replaced, and within that the request is re-sent every
// kernelInfoRetry.
var (
	kernelAnswerWait = 10 * time.Second
	kernelSocketWait = 2 * time.Second
	kernelInfoRetry  = 500 * time.Millisecond
)

// kernelSocket is one kernel websocket and the goroutine reading it.
type kernelSocket struct {
	conn    *wsclient.Conn
	session string
	msgs    chan inbound
	readErr chan error
	done    chan struct{}
}

func dialKernel(ctx context.Context, channelsURL func(kernelID, session string) string, kernelID string, hdr http.Header) (*kernelSocket, error) {
	session := newID()

	conn, err := wsclient.Dial(ctx, channelsURL(kernelID, session), wsclient.Options{Header: hdr})
	if err != nil {
		return nil, err
	}

	ks := &kernelSocket{conn: conn, session: session, msgs: make(chan inbound, 64),
		readErr: make(chan error, 1), done: make(chan struct{})}

	go readLoop(conn, ks.msgs, ks.readErr, ks.done)

	return ks, nil
}

// close stops the reader first, so it is never left blocked on a send, then the socket.
func (ks *kernelSocket) close() {
	close(ks.done)
	_ = ks.conn.Close()
}

// awaitKernel sends kernel_info_request until the kernel answers one on this socket, discarding
// whatever else arrives meanwhile, and gives up after kernelSocketWait.
//
// Why: against opensandbox/code-interpreter, about one fresh kernel socket in sixty is never
// answered - not the first message, not a re-sent one - and a cell sent on it sat on init and
// pings until the client gave up. A reply proves the path to the kernel is open; when none
// comes the caller replaces the socket, which is what recovers. jupyter_client's wait_for_ready
// handshakes the same way. Nothing is lost by discarding: no cell has been sent on this socket,
// so nothing arriving on it can belong to one.
func awaitKernel(ctx context.Context, ks *kernelSocket) error {
	giveUp := time.NewTimer(kernelSocketWait)
	defer giveUp.Stop()

	retry := time.NewTicker(kernelInfoRetry)
	defer retry.Stop()

	asked := map[string]bool{}

	ask := func() error {
		req := newKernelInfoRequest(ks.session)

		payload, err := json.Marshal(req)
		if err != nil {
			return err
		}

		asked[req.Header.MsgID] = true

		if err := ks.conn.WriteText(payload); err != nil {
			return fmt.Errorf("send kernel_info_request: %w", err)
		}

		return nil
	}

	if err := ask(); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-ks.readErr:
			return err
		case <-giveUp.C:
			return fmt.Errorf("no kernel_info_reply on this socket within %s", kernelSocketWait)
		case <-retry.C:
			if err := ask(); err != nil {
				return err
			}
		case m := <-ks.msgs:
			if m.Header.MsgType == "kernel_info_reply" && asked[m.ParentHeader.MsgID] {
				return nil
			}
		}
	}
}
