package jupyter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/wsclient/wstest"
)

// fakeJupyter is a Jupyter Server small enough to read: the REST calls the engine makes, and a
// channels websocket whose "kernel" runs a line-per-directive script instead of a language.
//
// Directives, one per line of the code:
//
//	print:<text>          stream stdout "<text>\n"
//	eprint:<text>         stream stderr
//	result:<text>         execute_result, text/plain
//	display:<mime>=<val>  display_data
//	raise:<name>:<value>  error with a two-line traceback
//	set:<k>=<v>           kernel-local variable
//	get:<k>               print its value, or "undefined"
//	sleep                 block until interrupted, then raise KeyboardInterrupt
//	die                   the kernel dies: status "restarting" with no parent, socket stays up
//	drop                  the socket drops mid-execution with no close frame
//	noise                 a stream message with a foreign parent (must not surface)
//	replyfirst            send execute_reply before the idle status, not after
//	noreply               never send execute_reply
//	replyerror            reply status error with no iopub error message
//	dropafteridle         go idle, then drop the socket before the reply
type fakeJupyter struct {
	t     *testing.T
	srv   *httptest.Server
	token string

	mu       sync.Mutex
	specs    map[string]string // kernelspec name -> language
	kernels  map[string]*fakeKernel
	sessions map[string]string // session id -> kernel id
	nextID   int

	sessionCreates atomic.Int32
	interrupts     atomic.Int32
	failSessions   atomic.Int32 // answer this many session creates with 503 first
	lastPath       atomic.Value // notebook path of the last session create
	lastKernelName atomic.Value
	running        chan string // kernel id, sent when a "sleep" starts

	// dropFirst swallows the first message a new socket sends, as a real Jupyter Server can
	// while it is still setting the connection up.
	dropFirst atomic.Bool
	// deadSockets is how many new sockets are never answered at all, the failure a real server
	// shows about once in sixty sockets.
	deadSockets atomic.Int32
	kernelInfos atomic.Int32
}

type fakeKernel struct {
	id        string
	mu        sync.Mutex
	vars      map[string]string
	count     int
	interrupt chan struct{}
}

func newFake(t *testing.T) *fakeJupyter {
	t.Helper()

	f := &fakeJupyter{
		t:        t,
		token:    "secret",
		specs:    map[string]string{"python3": "python", "bash": "bash"},
		kernels:  map[string]*fakeKernel{},
		sessions: map[string]string{},
		running:  make(chan string, 16),
	}

	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *fakeJupyter) engine(mod ...func(*Config)) *Engine {
	cfg := Config{BaseURL: f.srv.URL, Token: f.token}
	for _, m := range mod {
		m(&cfg)
	}

	return New(cfg)
}

func (f *fakeJupyter) id(prefix string) string {
	f.nextID++

	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeJupyter) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "token "+f.token
}

func (f *fakeJupyter) serve(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)

		return
	}

	p := r.URL.Path

	switch {
	case r.Method == http.MethodGet && p == "/api/kernelspecs":
		f.mu.Lock()

		ks := map[string]any{}
		for name, lang := range f.specs {
			ks[name] = map[string]any{"name": name, "spec": map[string]any{"language": lang, "display_name": name}}
		}

		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"default": "python3", "kernelspecs": ks})

	case r.Method == http.MethodPost && p == "/api/sessions":
		if f.failSessions.Load() > 0 {
			f.failSessions.Add(-1)
			http.Error(w, "starting", http.StatusServiceUnavailable)

			return
		}

		var in struct {
			Path   string `json:"path"`
			Type   string `json:"type"`
			Kernel struct {
				Name string `json:"name"`
			} `json:"kernel"`
		}

		_ = json.NewDecoder(r.Body).Decode(&in)
		f.lastPath.Store(in.Path)
		f.lastKernelName.Store(in.Kernel.Name)
		f.sessionCreates.Add(1)

		f.mu.Lock()
		if _, ok := f.specs[in.Kernel.Name]; !ok {
			f.mu.Unlock()
			http.Error(w, "no such kernel", http.StatusNotFound)

			return
		}

		k := &fakeKernel{id: f.id("kernel"), vars: map[string]string{}, interrupt: make(chan struct{}, 1)}
		sid := f.id("session")
		f.kernels[k.id] = k
		f.sessions[sid] = k.id
		f.mu.Unlock()

		writeJSON(w, http.StatusCreated, map[string]any{"id": sid, "path": in.Path, "type": in.Type,
			"kernel": map[string]any{"id": k.id, "name": in.Kernel.Name}})

	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/api/sessions/"):
		sid := strings.TrimPrefix(p, "/api/sessions/")

		f.mu.Lock()
		kid, ok := f.sessions[sid]
		delete(f.sessions, sid)
		delete(f.kernels, kid)
		f.mu.Unlock()

		if !ok {
			http.Error(w, "not found", http.StatusNotFound)

			return
		}

		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPost && strings.HasSuffix(p, "/interrupt"):
		kid := strings.TrimSuffix(strings.TrimPrefix(p, "/api/kernels/"), "/interrupt")

		f.mu.Lock()
		k, ok := f.kernels[kid]
		f.mu.Unlock()

		if !ok {
			http.Error(w, "not found", http.StatusNotFound)

			return
		}

		f.interrupts.Add(1)
		select {
		case k.interrupt <- struct{}{}:
		default:
		}

		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasSuffix(p, "/channels"):
		kid := strings.TrimSuffix(strings.TrimPrefix(p, "/api/kernels/"), "/channels")

		f.mu.Lock()
		k, ok := f.kernels[kid]
		f.mu.Unlock()

		if !ok {
			http.Error(w, "kernel not found", http.StatusNotFound)

			return
		}

		c, err := wstest.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()

		f.channels(c, k)

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// channels is one kernel websocket connection.
func (f *fakeJupyter) channels(c *wstest.Conn, k *fakeKernel) {
	send := func(channel, msgType string, parent string, content any) {
		m := map[string]any{
			"header":        map[string]any{"msg_id": fmt.Sprintf("srv-%d", rand63()), "msg_type": msgType},
			"parent_header": map[string]any{},
			"metadata":      map[string]any{},
			"content":       content,
			"channel":       channel,
		}
		if parent != "" {
			m["parent_header"] = map[string]any{"msg_id": parent, "msg_type": "execute_request"}
		}

		b, _ := json.Marshal(m)
		_ = c.WriteText(b)
	}

	// What a real server does on connect: a kernel_info probe of its own, whose status traffic
	// carries a parent the client never sent.
	send("iopub", "status", "server-probe", map[string]any{"execution_state": "busy"})
	send("iopub", "status", "server-probe", map[string]any{"execution_state": "idle"})

	dropNext := f.dropFirst.Load()
	dead := f.deadSockets.Add(-1) >= 0

	for {
		_, p, err := c.ReadMessage()
		if err != nil {
			return
		}

		if dropNext {
			dropNext = false
			continue
		}

		if dead {
			continue
		}

		var req struct {
			Header struct {
				MsgID   string `json:"msg_id"`
				MsgType string `json:"msg_type"`
			} `json:"header"`
			Channel string `json:"channel"`
			Content struct {
				Code string `json:"code"`
			} `json:"content"`
		}

		if json.Unmarshal(p, &req) == nil && req.Header.MsgType == "kernel_info_request" && req.Channel == "shell" {
			f.kernelInfos.Add(1)
			send("shell", "kernel_info_reply", req.Header.MsgID, map[string]any{"status": "ok", "protocol_version": "5.3"})

			continue
		}

		if json.Unmarshal(p, &req) != nil || req.Header.MsgType != "execute_request" || req.Channel != "shell" {
			f.t.Errorf("fake: unexpected message %s", p)

			return
		}

		if !f.execute(k, req.Header.MsgID, req.Content.Code, send, c) {
			return
		}
	}
}

// execute runs one cell. It returns false when the connection should end.
func (f *fakeJupyter) execute(k *fakeKernel, parent, code string, send func(string, string, string, any), c *wstest.Conn) bool {
	k.mu.Lock()
	k.count++
	count := k.count
	k.mu.Unlock()

	send("iopub", "status", parent, map[string]any{"execution_state": "busy"})
	send("iopub", "execute_input", parent, map[string]any{"code": code, "execution_count": count})

	status, replyFirst, noReply := "ok", false, false

	var dropAfterIdle bool

	var errReply map[string]any

	for _, line := range strings.Split(code, "\n") {
		verb, arg, _ := strings.Cut(strings.TrimSpace(line), ":")

		switch verb {
		case "print":
			send("iopub", "stream", parent, map[string]any{"name": "stdout", "text": arg + "\n"})
		case "eprint":
			send("iopub", "stream", parent, map[string]any{"name": "stderr", "text": arg + "\n"})
		case "result":
			send("iopub", "execute_result", parent, map[string]any{"execution_count": count,
				"data": map[string]any{"text/plain": arg}, "metadata": map[string]any{}})
		case "display":
			mime, val, _ := strings.Cut(arg, "=")
			send("iopub", "display_data", parent, map[string]any{"data": map[string]any{mime: val}, "metadata": map[string]any{}})
		case "raise":
			name, val, _ := strings.Cut(arg, ":")
			send("iopub", "error", parent, map[string]any{"ename": name, "evalue": val,
				"traceback": []string{"Traceback (most recent call last):", name + ": " + val}})

			status = "error"
		case "set":
			kk, v, _ := strings.Cut(arg, "=")
			k.mu.Lock()
			k.vars[kk] = v
			k.mu.Unlock()
		case "get":
			k.mu.Lock()
			v, ok := k.vars[arg]
			k.mu.Unlock()

			if !ok {
				v = "undefined"
			}

			send("iopub", "stream", parent, map[string]any{"name": "stdout", "text": v})
		case "sleep":
			f.running <- k.id
			<-k.interrupt
			send("iopub", "error", parent, map[string]any{"ename": "KeyboardInterrupt", "evalue": "",
				"traceback": []string{"KeyboardInterrupt"}})

			status = "error"
		case "die":
			f.running <- k.id
			send("iopub", "status", "", map[string]any{"execution_state": "restarting"})

			return true
		case "drop":
			_ = c.Conn.Close()

			return false
		case "noise":
			send("iopub", "stream", "someone-else", map[string]any{"name": "stdout", "text": "NOT MINE"})
		case "replyfirst":
			replyFirst = true
		case "noreply":
			noReply = true
		case "dropafteridle":
			noReply, dropAfterIdle = true, true
		case "replyerror":
			status = "error"
			errReply = map[string]any{"ename": "Aborted", "evalue": "reply only", "traceback": []string{"x"}}
		}
	}

	reply := map[string]any{"status": status, "execution_count": count}
	for k, v := range errReply {
		reply[k] = v
	}

	if replyFirst && !noReply {
		send("shell", "execute_reply", parent, reply)
	}

	send("iopub", "status", parent, map[string]any{"execution_state": "idle"})

	if dropAfterIdle {
		_ = c.Conn.Close()

		return false
	}

	if !replyFirst && !noReply {
		send("shell", "execute_reply", parent, reply)
	}

	return true
}

var randCounter atomic.Int64

func rand63() int64 { return randCounter.Add(1) }
