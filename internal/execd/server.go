//go:build unix

package execd

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aryanmehrotra/sbx/internal/execdctl"
	"github.com/aryanmehrotra/sbx/internal/jupyter"
)

// Error codes are upstream's (components/execd/pkg/web/model/error.go), so a client that
// switches on them behaves the same against either daemon.
const (
	codeInvalidRequest  = "INVALID_REQUEST_BODY"
	codeMissingQuery    = "MISSING_QUERY"
	codeRuntimeError    = "RUNTIME_ERROR"
	codeInvalidFile     = "INVALID_FILE"
	codeInvalidContent  = "INVALID_FILE_CONTENT"
	codeInvalidMetadata = "INVALID_FILE_METADATA"
	codeFileNotFound    = "FILE_NOT_FOUND"
	codeContextNotFound = "CONTEXT_NOT_FOUND"
	codeSessionNotFound = "SESSION_NOT_FOUND"
	codeNotSupported    = "NOT_SUPPORTED"
	codeUnauthorized    = "UNAUTHORIZED"
	codeNotFound        = "NOT_FOUND"
	codeRangeInvalid    = "RANGE_NOT_SATISFIABLE"
)

// Options configures a Server. The zero value serves without authentication, does not reap,
// and keeps background output under the system temp directory.
type Options struct {
	// AccessToken, when non-empty, must be presented in X-EXECD-ACCESS-TOKEN.
	AccessToken string

	// Reap makes the server the only caller of wait4 in the process, so it also reaps
	// orphans. Only for PID 1 or a subreaper; see procs.
	Reap bool

	// OutputDir holds background commands' output. Empty means a fresh private directory
	// under os.TempDir, removed by Close.
	OutputDir string

	// Logger receives one line per notable event. Nil means the standard logger.
	Logger *log.Logger

	// Jupyter runs the /code routes. Nil means an engine configured from JUPYTER_HOST,
	// JUPYTER_TOKEN and JUPYTER_PORT, which answers 501 with the reason when they name nothing.
	Jupyter *jupyter.Engine

	// JupyterStartupWait bounds how long a /code call waits for a configured Jupyter that is not
	// answering yet, as when the image entrypoint is still starting it. Zero means 30s.
	JupyterStartupWait time.Duration

	// ControlSecret authorises /sbx/seal and /sbx/rekey, the host's calls around a snapshot. Empty
	// means both are refused: with no secret there is no host to take a re-key from. See
	// internal/execdctl for the protocol.
	ControlSecret string

	// ControlOverVsockOnly refuses the control endpoints on any connection that did not arrive
	// over vsock. Set when execd serves vsock: then the host is on the other end of that, and the
	// TCP listener is reachable by everything on the sandbox's network.
	ControlOverVsockOnly bool
}

// Server is the execd HTTP API. Build it with New, serve it with any http.Server, and Close it
// to kill what it started.
type Server struct {
	// auth is the current access token and the context every request it authorised runs under.
	// Swapped whole - by a warm-pool claim or a restore's re-key - while requests are being served,
	// so a request always checks a token and inherits the lifetime of the same generation.
	auth    atomic.Pointer[authState]
	claimed atomic.Bool

	// sealed refuses every client call until a re-key; see rekey.go.
	sealed atomic.Bool
	ctl    control

	procs *procs
	log   *log.Logger
	mux   *http.ServeMux
	code  *jupyter.Engine

	// codeUp remembers that Jupyter answered, so /code calls skip the probe; codeWait is how
	// long a call waits for a configured Jupyter that is still starting. See codeReady.
	codeUp   atomic.Bool
	codeWait time.Duration

	// proxyH is /proxy/, guarded; see proxy for why it bypasses the mux.
	proxyH http.Handler

	outputDir     string
	ownsOutputDir bool

	mu       sync.Mutex
	commands map[string]*command
	sessions map[string]*session
	ptys     map[string]*ptySession

	stopJanitor chan struct{}
	closeOnce   sync.Once
}

// New builds a Server. It fails only when the output directory cannot be made.
func New(o Options) (*Server, error) {
	s := &Server{
		procs:       newProcs(o.Reap),
		log:         o.Logger,
		commands:    map[string]*command{},
		sessions:    map[string]*session{},
		ptys:        map[string]*ptySession{},
		stopJanitor: make(chan struct{}),
		code:        o.Jupyter,
	}

	if s.code == nil {
		s.code = jupyter.New(jupyter.ConfigFromEnv())
	}

	s.codeWait = o.JupyterStartupWait
	if s.codeWait <= 0 {
		s.codeWait = 30 * time.Second
	}

	if s.log == nil {
		s.log = log.New(os.Stderr, "execd: ", log.LstdFlags)
	}

	s.outputDir = o.OutputDir
	if s.outputDir == "" {
		// Private (0700) and per process, so another user in the sandbox cannot read a
		// command's output and a restarted execd never trips over an old one's files.
		dir, err := os.MkdirTemp("", "sbx-execd-")
		if err != nil {
			return nil, fmt.Errorf("create the directory for background command output under %s: %w "+
				"(set TMPDIR to a writable directory)", os.TempDir(), err)
		}

		s.outputDir, s.ownsOutputDir = dir, true
	} else if err := os.MkdirAll(s.outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("create output directory %s: %w", s.outputDir, err)
	}

	tok := []byte(o.AccessToken)
	s.auth.Store(newAuth(tok))

	s.ctl.secret = []byte(o.ControlSecret)
	s.ctl.vsockOnly = o.ControlOverVsockOnly
	s.ctl.identityEnv = map[string]bool{}

	// A server with no token was never a pool member, so there is nothing to claim.
	s.claimed.Store(len(tok) == 0)

	s.routes()

	go s.janitor(time.Hour, commandRetention)

	return s, nil
}

// Close kills every command and session the server started and stops its background work. It
// does not stop an http.Server serving it; that is the caller's.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.stopJanitor)

		s.mu.Lock()
		var groups []int

		for _, c := range s.commands {
			if pgid := c.runningGroup(); pgid > 0 {
				groups = append(groups, pgid)
			}
		}

		for _, ss := range s.sessions {
			if pgid := ss.currentGroup(); pgid > 0 {
				groups = append(groups, pgid)
			}
		}

		ptys := make([]*ptySession, 0, len(s.ptys))
		for _, ps := range s.ptys {
			ptys = append(ptys, ps)
		}
		s.mu.Unlock()

		for _, g := range groups {
			_ = signalGroup(g, syscall.SIGKILL)
		}

		for _, ps := range ptys {
			ps.close()
		}

		s.procs.close()

		if s.ownsOutputDir {
			_ = os.RemoveAll(s.outputDir)
		}
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, proxyPrefix) {
		s.proxyH.ServeHTTP(w, r)
		return
	}

	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	m := http.NewServeMux()

	handle := func(pattern string, h http.HandlerFunc) {
		m.Handle(pattern, s.guard(h))
	}

	// /ping answers without the token, as upstream does: it is the liveness probe, and the
	// thing probing it (sbx serve, a load balancer) is not necessarily holding the token.
	//
	// Sealed, it says 503: a sandbox waiting for its re-key is not serving, and the wake proxy
	// must not be told otherwise.
	m.Handle("GET /ping", recoverer(s.log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if s.sealed.Load() {
			writeSealed(w)
			return
		}

		w.WriteHeader(http.StatusOK)
	})))

	// The host's calls around a snapshot. Not behind guard: they are authorised by the control
	// secret, not the access token they replace, and must work while sealed.
	m.Handle("POST "+execdctl.PathSeal, recoverer(s.log, http.HandlerFunc(s.seal)))
	m.Handle("POST "+execdctl.PathRekey, recoverer(s.log, http.HandlerFunc(s.rekey)))

	handle("POST /sbx/claim", s.claim)

	handle("POST /command", s.runCommand)
	handle("DELETE /command", s.interrupt)
	// /command/status/{id} and /command/{id}/logs overlap for the path /command/status/logs,
	// and ServeMux refuses to register two patterns where neither is more specific. One
	// pattern, told apart by hand.
	handle("GET /command/{a}/{b}", s.commandSubresource)

	handle("POST /session", s.createSession)
	handle("POST /session/{sessionId}/run", s.runInSession)
	handle("DELETE /session/{sessionId}", s.deleteSession)

	handle("GET /files/info", s.filesInfo)
	handle("DELETE /files", s.removeFiles)
	handle("POST /files/permissions", s.chmodFiles)
	handle("POST /files/mv", s.moveFiles)
	handle("GET /files/search", s.searchFiles)
	handle("POST /files/replace", s.replaceContent)
	handle("POST /files/upload", s.uploadFiles)
	handle("GET /files/download", s.downloadFile)

	handle("GET /directories/list", s.listDirectory)
	handle("POST /directories", s.makeDirs)
	handle("DELETE /directories", s.removeDirs)

	handle("GET /metrics", s.metrics)
	handle("GET /metrics/watch", s.watchMetrics)

	handle("POST /code", s.runCode)
	handle("DELETE /code", s.interruptCode)
	handle("POST /code/context", s.createCodeContext)
	handle("GET /code/contexts", s.listCodeContexts)
	handle("DELETE /code/contexts", s.deleteCodeContexts)
	handle("GET /code/contexts/{contextId}", s.getCodeContext)
	handle("DELETE /code/contexts/{contextId}", s.deleteCodeContext)

	handle("POST /pty", s.createPTY)
	handle("GET /pty/{sessionId}", s.getPTY)
	handle("DELETE /pty/{sessionId}", s.deletePTY)
	handle("GET /pty/{sessionId}/ws", s.ptyWebSocket)

	// Parts of the API a later release adds. They answer 501 with the spec's error shape, not
	// 404: a 404 reads as "you have the path wrong", and a client should instead learn that
	// this daemon knows the endpoint and does not do it yet.
	for pattern, release := range notYet {
		handle(pattern, notImplemented(pattern, release))
	}

	handle("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound,
			fmt.Sprintf("%s %s is not an execd endpoint; the API is specs/execd-api.yaml in OpenSandbox release-1.1.0",
				r.Method, r.URL.Path))
	})

	s.mux = m
	s.proxyH = s.guard(s.proxy)
}

// notYet maps each unimplemented prefix to the sbx release that implements it, per the release
// table in docs/superpowers/specs/2026-09-25-opensandbox-compat-design.md.
var notYet = map[string]string{
	"/v1/isolated/": "v0.11.0 (isolated sessions)",
}

func notImplemented(pattern, release string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotImplemented, codeNotSupported,
			fmt.Sprintf("%s %s is not supported by this sbx execd yet; sbx %s adds it", r.Method, r.URL.Path, release))
	}
}

// guard is authentication plus panic recovery, in front of every endpoint but /ping.
func (s *Server) guard(h http.HandlerFunc) http.Handler {
	inner := recoverer(s.log, h)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sealed before the auth check, and for everyone: this is the state a snapshot captures,
		// and what a clone must answer until its host has re-keyed it.
		if s.sealed.Load() {
			writeSealed(w)
			return
		}

		// One load: the token checked and the lifetime inherited are the same generation.
		a := s.auth.Load()

		if tok := a.token; len(tok) > 0 {
			got := r.Header.Get(AccessTokenHeader)
			// Constant time, so the token cannot be recovered a byte at a time from how long a
			// wrong guess takes. Upstream compares with != ; this is the one place sbx is
			// stricter on purpose.
			if got == "" || subtle.ConstantTimeCompare([]byte(got), tok) != 1 {
				writeError(w, http.StatusUnauthorized, codeUnauthorized,
					"invalid or missing header "+AccessTokenHeader+
						"; use the token returned with this sandbox's execd endpoint")

				return
			}
		}

		// A re-key that replaces this token ends this request, as a dropped client would. In a
		// restored clone that client's connection is already gone; in a live sandbox it is
		// someone whose credential was just revoked.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		stop := context.AfterFunc(a.ctx, cancel)
		defer stop()

		inner.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverer turns a panic in a handler into a 500 instead of a dropped connection. A handler
// that has already started streaming cannot change its status, so there the connection is
// simply ended - which the client sees as a truncated stream, the honest description.
func recoverer(l *log.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}

		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}

				l.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, v)

				if !tw.wrote {
					writeError(tw, http.StatusInternalServerError, codeRuntimeError,
						fmt.Sprintf("execd failed while serving %s %s: %v; this is a bug in sbx execd", r.Method, r.URL.Path, v))
				}
			}
		}()

		h.ServeHTTP(tw, r)
	})
}

// trackingWriter remembers whether headers went out, for recoverer.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush on the real writer.
func (t *trackingWriter) Unwrap() http.ResponseWriter {
	return t.ResponseWriter
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Code: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		b, _ = json.Marshal(errorBody{Code: codeRuntimeError, Message: "encode response: " + err.Error()})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// newID is 32 hex characters, the shape of upstream's ids (a UUID without dashes).
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}
