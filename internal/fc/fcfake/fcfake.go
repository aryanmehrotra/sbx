// Package fcfake is a Firecracker API server that keeps state and never boots anything.
//
// It exists so the driver and the provider state machine are tested against the shape of the
// real API - which calls are legal when, what a refusal looks like, what a snapshot leaves on
// disk - on a Mac, in CI, and anywhere else without /dev/kvm. Its rules are the ones the pinned
// v1.17.0 enforces and the spike exercised: configuration is pre-boot only, a snapshot needs a
// paused VM, a Diff needs dirty-page tracking, and a load goes to a process configured with
// nothing else. It is not a place to invent behaviour the real VMM lacks; where a rule here is
// looser than Firecracker's, the e2e test (SBX_FC_E2E=1) is the one that says so.
package fcfake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Call is one request the fake received.
type Call struct {
	Method string
	Path   string
	Body   map[string]any
}

// Server is one fake firecracker process.
type Server struct {
	mu    sync.Mutex
	calls []Call

	// Fail makes the next request to a path answer 400 with this fault_message, once.
	Fail map[string]string

	// Stall holds the next request to a path this long before answering, once: a VMM that is
	// alive and slow, which is not the same as one that is gone.
	Stall map[string]time.Duration

	configured bool // anything pre-boot has been PUT
	started    bool
	paused     bool
	dirty      bool // machine-config track_dirty_pages, or carried by a load
	boot       map[string]any
	drives     map[string]map[string]any
	ifaces     map[string]map[string]any
	vsock      map[string]any
	machine    map[string]any

	ln  net.Listener
	srv *http.Server
}

// StallNext is Stall set under the server's lock, safe while it is serving.
func (s *Server) StallNext(path string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Stall[path] = d
}

// Start serves a fresh fake on the unix socket at sock.
func Start(sock string) (*Server, error) {
	_ = os.Remove(sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}

	s := &Server{
		Fail:   map[string]string{},
		Stall:  map[string]time.Duration{},
		drives: map[string]map[string]any{},
		ifaces: map[string]map[string]any{},
		ln:     ln,
	}
	s.srv = &http.Server{Handler: s}

	go func() { _ = s.srv.Serve(ln) }()

	return s, nil
}

// Close stops serving and removes the socket, as a killed firecracker leaves it (closed).
func (s *Server) Close() error {
	err := s.srv.Close()
	_ = os.Remove(s.ln.Addr().String())

	return err
}

// Calls returns every request so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Call(nil), s.calls...)
}

// Paths returns "METHOD /path" for every request so far, the shape tests assert on.
func (s *Server) Paths() []string {
	var out []string
	for _, c := range s.Calls() {
		out = append(out, c.Method+" "+c.Path)
	}

	return out
}

// State is what GET / would report.
func (s *Server) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.state()
}

func (s *Server) state() string {
	switch {
	case !s.started:
		return "Not started"
	case s.paused:
		return "Paused"
	default:
		return "Running"
	}
}

const notAfterBoot = "The requested operation is not supported after starting the microVM."

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var body map[string]any

	raw, _ := io.ReadAll(r.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			fault(w, 400, "Syntax error: "+err.Error())
			return
		}
	}

	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.Path, Body: body})

	if d, ok := s.Stall[r.URL.Path]; ok {
		delete(s.Stall, r.URL.Path)
		time.Sleep(d)
	}

	if f, ok := s.Fail[r.URL.Path]; ok {
		delete(s.Fail, r.URL.Path)
		fault(w, 400, f)

		return
	}

	key := r.Method + " " + r.URL.Path

	switch {
	case key == "GET /":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": "anonymous-instance", "state": s.state(), "vmm_version": "1.17.0", "app_name": "Firecracker",
		})
	case r.Method == http.MethodPut && isPreBoot(r.URL.Path):
		if s.started {
			fault(w, 400, notAfterBoot)
			return
		}

		s.preBoot(r.URL.Path, body)
		w.WriteHeader(204)
	case key == "PUT /actions":
		s.action(w, body)
	case key == "PATCH /vm":
		s.vm(w, body)
	case key == "PUT /snapshot/create":
		s.create(w, body)
	case key == "PUT /snapshot/load":
		s.load(w, body)
	default:
		fault(w, 400, "Invalid request method and/or path: "+key)
	}
}

func isPreBoot(p string) bool {
	switch {
	case p == "/boot-source", p == "/machine-config", p == "/vsock", p == "/entropy":
		return true
	case strings.HasPrefix(p, "/drives/"), strings.HasPrefix(p, "/network-interfaces/"):
		return true
	}

	return false
}

func (s *Server) preBoot(p string, body map[string]any) {
	s.configured = true

	switch {
	case p == "/boot-source":
		s.boot = body
	case p == "/machine-config":
		s.machine = body
		s.dirty, _ = body["track_dirty_pages"].(bool)
	case p == "/vsock":
		s.vsock = body
	case strings.HasPrefix(p, "/drives/"):
		s.drives[strings.TrimPrefix(p, "/drives/")] = body
	case strings.HasPrefix(p, "/network-interfaces/"):
		s.ifaces[strings.TrimPrefix(p, "/network-interfaces/")] = body
	}
}

func (s *Server) action(w http.ResponseWriter, body map[string]any) {
	switch body["action_type"] {
	case "InstanceStart":
		if s.started {
			fault(w, 400, notAfterBoot)
			return
		}

		if s.boot == nil {
			fault(w, 400, "Cannot start microvm without kernel configuration.")
			return
		}

		s.started = true
	case "SendCtrlAltDel":
		if !s.started {
			fault(w, 400, "The requested operation is not supported before starting the microVM.")
			return
		}
	default:
		fault(w, 400, fmt.Sprintf("unknown action %v", body["action_type"]))
		return
	}

	w.WriteHeader(204)
}

func (s *Server) vm(w http.ResponseWriter, body map[string]any) {
	if !s.started {
		fault(w, 400, "The requested operation is not supported before starting the microVM.")
		return
	}

	switch body["state"] {
	case "Paused":
		s.paused = true
	case "Resumed":
		s.paused = false
	default:
		fault(w, 400, fmt.Sprintf("unknown state %v", body["state"]))
		return
	}

	w.WriteHeader(204)
}

// snapshotState is what the fake writes as vm.state: enough to restore its own config.
type snapshotState struct {
	Boot    map[string]any            `json:"boot"`
	Drives  map[string]map[string]any `json:"drives"`
	Ifaces  map[string]map[string]any `json:"ifaces"`
	Vsock   map[string]any            `json:"vsock"`
	Machine map[string]any            `json:"machine"`
	Type    string                    `json:"type"`
}

func (s *Server) create(w http.ResponseWriter, body map[string]any) {
	if !s.started || !s.paused {
		fault(w, 400, "Cannot create a snapshot while the microVM is running; pause it first.")
		return
	}

	typ, _ := body["snapshot_type"].(string)
	if typ == "" {
		typ = "Full"
	}

	if typ == "Diff" && !s.dirty {
		fault(w, 400, "Diff snapshots are not enabled: dirty page tracking is off.")
		return
	}

	statePath, _ := body["snapshot_path"].(string)
	memPath, _ := body["mem_file_path"].(string)

	st, _ := json.Marshal(snapshotState{s.boot, s.drives, s.ifaces, s.vsock, s.machine, typ})
	if err := os.WriteFile(statePath, st, 0o600); err != nil {
		fault(w, 400, "Cannot write snapshot state: "+err.Error())
		return
	}

	// A marker, not 128 MiB: the fake records which kind of memory file it was.
	if err := os.WriteFile(memPath, []byte("mem:"+typ), 0o600); err != nil {
		fault(w, 400, "Cannot write memory file: "+err.Error())
		return
	}

	w.WriteHeader(204)
}

func (s *Server) load(w http.ResponseWriter, body map[string]any) {
	if s.configured || s.started {
		fault(w, 400, "Loading a microVM snapshot not allowed after configuring boot-specific resources.")
		return
	}

	statePath, _ := body["snapshot_path"].(string)

	raw, err := os.ReadFile(statePath)
	if err != nil {
		fault(w, 400, "Cannot open snapshot file: "+err.Error())
		return
	}

	var st snapshotState
	if err := json.Unmarshal(raw, &st); err != nil {
		fault(w, 400, "Cannot deserialize the microVM state: "+err.Error())
		return
	}

	mb, _ := body["mem_backend"].(map[string]any)
	if p, _ := mb["backend_path"].(string); p == "" {
		fault(w, 400, "missing mem_backend")
		return
	} else if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		fault(w, 400, "Cannot open the memory file: "+err.Error())
		return
	}

	s.boot, s.drives, s.ifaces, s.vsock, s.machine = st.Boot, st.Drives, st.Ifaces, st.Vsock, st.Machine

	if ov, ok := body["vsock_override"].(map[string]any); ok && s.vsock != nil {
		s.vsock["uds_path"] = ov["uds_path"]
	}

	if ovs, ok := body["network_overrides"].([]any); ok {
		for _, o := range ovs {
			m, _ := o.(map[string]any)
			id, _ := m["iface_id"].(string)

			iface, ok := s.ifaces[id]
			if !ok {
				fault(w, 400, "Invalid network override: no interface "+id)
				return
			}

			iface["host_dev_name"] = m["host_dev_name"]
		}
	}

	s.dirty, _ = body["track_dirty_pages"].(bool)
	s.started = true
	s.paused = true

	if resume, _ := body["resume_vm"].(bool); resume {
		s.paused = false
	}

	w.WriteHeader(204)
}

// Iface returns the interface config the fake currently holds, for asserting overrides.
func (s *Server) Iface(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ifaces[id]
}

// VsockPath returns the vsock uds_path the fake currently holds.
func (s *Server) VsockPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, _ := s.vsock["uds_path"].(string)

	return p
}

func fault(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"fault_message": msg})
}
