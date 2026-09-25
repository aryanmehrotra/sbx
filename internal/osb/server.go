// Package osb serves OpenSandbox's lifecycle API from inside `sbx serve`.
//
// Any OpenSandbox client - the SDKs, the osb CLI, the MCP server - pointed at sbx creates,
// lists, pauses, renews and deletes sandboxes here, and talks to `sbx execd` inside each one.
// The contract is upstream's own spec at release-1.1.0 (specs/sandbox-lifecycle.yml and
// specs/diagnostic-api.yml): paths, field names, status codes and the {code, message} error
// body. Where sbx does not do something yet, the endpoint exists and says so with a 501 naming
// the release that adds it - never a 404 that reads as "wrong server", never a 200 that did
// nothing.
//
// Everything here goes through the Provider and the daemon, not around them. A sandbox created
// through the API is an ordinary sbx sandbox named for its id: `sbx list` shows it, `sbx logs`
// reads it, and the daemon fronts, freezes and wakes it the way it does everything else.
package osb

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/slotlock"
)

// Runtime is what the API needs from the daemon: the one thing allowed to start and stop
// sandboxes. *daemon implements it; tests use a fake.
type Runtime interface {
	// Refresh makes the daemon notice a sandbox now, so its endpoint answers immediately.
	Refresh(ctx context.Context)

	// Freeze pauses a sandbox and holds it until Thaw; traffic does not undo it.
	Freeze(ctx context.Context, sandbox string) error

	// Thaw releases a hold and brings the sandbox back.
	Thaw(ctx context.Context, sandbox string) error

	// Hold re-asserts (or drops) a pause without touching the container - used on start, for
	// pauses that outlived a daemon restart, and on delete.
	Hold(sandbox string, held bool)
}

// Options configures a Server. Provider and Runtime are required.
type Options struct {
	Provider provider.Provider
	Runtime  Runtime

	// Key is the one operator key. Empty means no authentication, which Serve only allows on a
	// loopback address.
	Key string

	// Owner is written on every container this API creates, as the sbx.osb label: which daemon
	// made it. Its presence is what matters - a daemon that does not serve the API leaves labelled
	// containers alone - and the value says whose they are to a person reading docker inspect.
	// Empty is "sbx-serve".
	Owner string

	// StateDir holds one file per sandbox. Default ~/.sbx/osb.
	StateDir string

	// Version is this build's version: it names the execd volume and the activator image.
	Version string

	// ReadyTimeout bounds Pending: from the container existing to execd answering /ping.
	ReadyTimeout time.Duration

	// ReapEvery is how often expiry is checked.
	ReapEvery time.Duration

	// Seams for tests. Zero values are the real thing.
	Now       func() time.Time
	Ping      func(ctx context.Context, hostport string) error
	Execd     func(ctx context.Context, arch string) (execdSource, error)
	LockSlots func() func()

	// NewID mints sandbox ids; it must return osb-<12 hex>. A test sharing an engine with other
	// API sandboxes uses it to give its own a prefix it can scope a daemon to.
	NewID func() string

	// Egress changes a running sandbox's egress policy - the daemon's EgressControl. Nil means
	// this server cannot, and the networkpolicy routes and create-time rules say so with 501.
	Egress EgressAPI

	// EgressStatus maps an Egress error to its HTTP status (daemon.EgressHTTPStatus).
	EgressStatus func(error) int
}

// EgressAPI is the live policy of a sandbox's egress filter. *daemon.EgressControl implements
// it; it is an interface only because this package cannot import the daemon that imports it.
type EgressAPI interface {
	GetPolicy(ctx context.Context, sandbox, service string) (egress.Status, error)
	SetPolicy(ctx context.Context, sandbox, service string, p egress.Policy) (egress.Status, error)
	PatchPolicy(ctx context.Context, sandbox, service string, rules []egress.Rule) (egress.Status, error)
	DeleteRules(ctx context.Context, sandbox, service string, targets []string) (egress.Status, error)
	Handler(sandbox string) http.Handler
	Forget(sandbox string) error
}

// Server is the API. Create one with New; mount Handler; run Run for expiry.
type Server struct {
	p       provider.Provider
	rt      Runtime
	key     string
	owner   string
	store   store
	version string

	readyTimeout time.Duration
	reapEvery    time.Duration

	now       func() time.Time
	ping      func(ctx context.Context, hostport string) error
	execd     func(ctx context.Context, arch string) (execdSource, error)
	lockSlots func() func()
	newID     func() string

	egress       EgressAPI
	egressStatus func(error) int

	// base outlives any one request: provisioning continues after the create call has
	// returned its 202, which is the whole point of Pending.
	base   context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	recs map[string]*record

	// provisioning cancels an in-flight create, so a DELETE during Pending stops it.
	provisioning map[string]context.CancelFunc

	// seeded remembers volumes already proven to run, so only the first create per volume pays
	// for the check.
	seedMu sync.Mutex
	seeded map[string]bool

	wg sync.WaitGroup
}

// New loads existing records and returns a Server. It does not start the reaper; call Run.
func New(o Options) (*Server, error) {
	if o.Provider == nil || o.Runtime == nil {
		return nil, errors.New("osb: a provider and a runtime are required")
	}

	if o.StateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("osb: no home directory for the sandbox state (%v) - pass a state dir", err)
		}

		o.StateDir = filepath.Join(home, ".sbx", "osb")
	}

	s := &Server{
		p:            o.Provider,
		rt:           o.Runtime,
		key:          o.Key,
		owner:        o.Owner,
		store:        store{dir: o.StateDir},
		version:      o.Version,
		readyTimeout: o.ReadyTimeout,
		reapEvery:    o.ReapEvery,
		now:          o.Now,
		ping:         o.Ping,
		execd:        o.Execd,
		lockSlots:    o.LockSlots,
		recs:         map[string]*record{},
		provisioning: map[string]context.CancelFunc{},
		seeded:       map[string]bool{},
	}

	if s.readyTimeout <= 0 {
		s.readyTimeout = 2 * time.Minute
	}

	if s.reapEvery <= 0 {
		s.reapEvery = 10 * time.Second
	}

	if s.now == nil {
		s.now = time.Now
	}

	if s.ping == nil {
		s.ping = pingExecd
	}

	if s.execd == nil {
		s.execd = newExecdResolver(o.Version).resolve
	}

	if s.lockSlots == nil {
		s.lockSlots = slotlock.Lock
	}

	s.egress, s.egressStatus = o.Egress, o.EgressStatus
	if s.egressStatus == nil {
		s.egressStatus = func(error) int { return http.StatusInternalServerError }
	}

	s.newID = o.NewID
	if s.newID == nil {
		s.newID = newID
	}

	s.base, s.cancel = context.WithCancel(context.Background())

	recs, errs := s.store.all()
	for _, err := range errs {
		logs.Default.Warn("", "", "osb: %v", err)
	}

	for _, r := range recs {
		s.recs[r.ID] = r

		// Here, synchronously, and not only in Run: the daemon binds its listeners on its first
		// discovery, and Run is a goroutine racing it. A hold that arrived after the listener
		// would leave a window where a connection thaws a sandbox the API still says is Paused.
		if r.PausedByAPI {
			s.rt.Hold(r.ID, true)
		}
	}

	return s, nil
}

// Close stops provisioning goroutines and waits for them. Sandboxes are left as they are - the
// daemon stopping is not a reason to tear anything down.
func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
}

// Run recovers what a restart interrupted, then checks expiry until ctx is done.
func (s *Server) Run(ctx context.Context) {
	s.recover(ctx)
	s.reap(ctx)

	t := time.NewTicker(s.reapEvery)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reap(ctx)
		}
	}
}

// recover puts back what only lived in memory: pause holds (the daemon forgets them when it
// restarts, and a paused sandbox must not be thawed by the first request after an upgrade) and
// readiness waits for sandboxes that were still Pending.
func (s *Server) recover(ctx context.Context) {
	s.mu.Lock()

	var pending []*record

	for _, r := range s.recs {
		if r.PausedByAPI {
			s.rt.Hold(r.ID, true)
		}

		if r.State == statePending {
			pending = append(pending, r)
		}
	}
	s.mu.Unlock()

	if len(pending) == 0 {
		return
	}

	units, err := s.p.List(ctx, "")
	if err != nil {
		logs.Default.Warn("", "", "osb: could not list sandboxes to recover Pending ones: %v", err)
		return
	}

	have := map[string]bool{}
	for _, u := range units {
		have[u.Sandbox] = true
	}

	for _, r := range pending {
		if !have[r.ID] {
			s.update(r.ID, func(r *record) {
				r.transition(stateFailed, "interrupted", "sbx serve restarted before this "+
					"sandbox's container was created; delete it and create it again", s.now())
			})

			continue
		}

		id := r.ID
		pctx, cancel := context.WithCancel(s.base)

		s.mu.Lock()
		s.provisioning[id] = cancel
		s.mu.Unlock()

		s.wg.Add(1)

		go func() {
			defer s.wg.Done()
			defer cancel()

			s.waitReady(pctx, id)
		}()
	}
}

// Handler is the API, authentication included.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	})

	mux.HandleFunc("POST /v1/sandboxes", s.create)
	mux.HandleFunc("GET /v1/sandboxes", s.list)
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.get)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.delete)
	mux.HandleFunc("PATCH /v1/sandboxes/{id}/metadata", s.patchMetadata)
	mux.HandleFunc("POST /v1/sandboxes/{id}/pause", s.pause)
	mux.HandleFunc("POST /v1/sandboxes/{id}/resume", s.resume)
	mux.HandleFunc("POST /v1/sandboxes/{id}/renew-expiration", s.renew)
	mux.HandleFunc("GET /v1/sandboxes/{id}/endpoints/{port}", s.endpoint)
	mux.HandleFunc("GET /v1/sandboxes/{id}/diagnostics/logs", s.diagnostics("logs"))
	mux.HandleFunc("GET /v1/sandboxes/{id}/diagnostics/events", s.diagnostics("events"))
	mux.HandleFunc("POST /v1/metrics/events", s.metrics)

	mux.HandleFunc("GET /v1/sandboxes/{id}/networkpolicy", s.networkPolicy)
	mux.HandleFunc("PUT /v1/sandboxes/{id}/networkpolicy", s.networkPolicy)
	mux.HandleFunc("PATCH /v1/sandboxes/{id}/networkpolicy", s.networkPolicy)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/networkpolicy", s.networkPolicy)
	mux.HandleFunc("/v1/sandboxes/{id}/egress/policy", s.sidecarPolicy)

	// Declared, and refused with the release that brings them. A client probing for snapshot
	// support gets a precise "not yet", which is something it can report; a 404 would read as
	// a misconfigured base URL.
	for _, route := range []struct{ pattern, what string }{
		{"/v1/snapshots", "snapshots"},
		{"/v1/snapshots/{sid}", "snapshots"},
		{"/v1/sandboxes/{id}/snapshots", "snapshots"},
		{"/v1/templates", "templates"},
		{"/v1/templates/{tid}", "templates"},
	} {
		what := route.what
		mux.HandleFunc(route.pattern, func(w http.ResponseWriter, _ *http.Request) {
			notYet(w, what, "v0.10.0")
		})
	}

	return s.withRequestID(s.authed(jsonFallbacks(mux)))
}

// authed checks the one operator key, from either header the SDKs send: OPEN-SANDBOX-API-KEY
// directly, X-API-Key in their server-proxy mode.
func (s *Server) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The sidecar-style policy route carries its own per-sandbox credential instead - the SDK
		// sends only the endpoint's headers there, not the API key - and checks it itself.
		sidecar := r.Header.Get(egressAuthHeader) != "" && isSidecarPath(r.URL.Path)

		if s.key == "" || sidecar || (r.Method == http.MethodGet && r.URL.Path == "/health") {
			next.ServeHTTP(w, r)
			return
		}

		got := r.Header.Get("OPEN-SANDBOX-API-KEY")
		if got == "" {
			got = r.Header.Get("X-API-Key")
		}

		// Checked before comparing: ConstantTimeCompare("", "") is equal, and s.key is never
		// empty here, but a missing header deserves its own message anyway.
		if got == "" {
			writeErr(w, http.StatusUnauthorized, "MISSING_API_KEY",
				"authentication credentials are missing: send the key in the OPEN-SANDBOX-API-KEY header")

			return
		}

		if subtle.ConstantTimeCompare([]byte(got), []byte(s.key)) != 1 {
			writeErr(w, http.StatusUnauthorized, "INVALID_API_KEY",
				"authentication credentials are invalid: check the key this sbx serve was started with (--osb-key, SBX_OSB_KEY, or the one it generated in ~/.sbx/osb/key)")

			return
		}

		next.ServeHTTP(w, r)
	})
}

// withRequestID echoes the caller's X-Request-ID, or mints one, on every response - the SDKs
// attach it to errors so a failure can be matched to a log line.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}

		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

// jsonFallbacks turns the mux's own plain-text 404 and 405 into the spec's error body, which
// the spec requires of every non-2xx response.
func jsonFallbacks(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}

		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			probe := r.Clone(r.Context())
			probe.Method = m

			if _, pattern := mux.Handler(probe); pattern != "" {
				writeErr(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
					fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path))

				return
			}
		}

		writeErr(w, http.StatusNotFound, "NOT_FOUND",
			fmt.Sprintf("no such endpoint %s %s - the API is under /v1", r.Method, r.URL.Path))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Code: code, Message: msg})
}

// notYet is the capability refusal: 501, OpenSandbox's own error shape, and the release that
// adds it, so the caller can tell "this server cannot" from "this server is broken".
func notYet(w http.ResponseWriter, what, release string) {
	writeErr(w, http.StatusNotImplemented, "SANDBOX::API_NOT_SUPPORTED",
		fmt.Sprintf("%s are not supported by this sbx yet; they arrive in sbx %s", what, release))
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)

	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)

	return "osb-" + hex.EncodeToString(b)
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}

// CheckBind refuses any address but loopback, key or no key. The endpoints this API hands out
// are 127.0.0.1 listeners on this machine, so a client on another one would be given addresses
// it cannot dial; carrying them over the API's own port is server-proxy mode, which is not
// built. Until it is, a non-loopback bind would only put the key and every request on the
// network for a client that could not use the answer. key is kept for the callers' sake: the
// key is required separately (see the daemon's osbKey), and loopback is never a reason to drop it.
func CheckBind(addr, _ string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--osb-addr %q is not host:port - try 127.0.0.1:8080", addr)
	}

	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())

	if !loopback {
		return fmt.Errorf("--osb-addr %s is not a loopback address, and sbx does not serve the "+
			"OpenSandbox API off this machine yet: the sandbox endpoints it hands out are "+
			"127.0.0.1 listeners here, and server-proxy mode, which would carry them, is not "+
			"built. Bind 127.0.0.1:%s and reach it from another machine through a tunnel - "+
			"`ssh -L %s:127.0.0.1:%s <this host>` for the API, plus `sbx connect` to a "+
			"`sbx serve --connect-addr` here for the sandbox endpoints", addr, port, port, port)
	}

	return nil
}

// pingExecd asks execd whether it is up, through the address the caller will be given - the
// daemon's wake port - so Running means the endpoint handed out actually answers.
func pingExecd(ctx context.Context, hostport string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+hostport+"/ping", nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("execd /ping answered %s", resp.Status)
	}

	return nil
}
