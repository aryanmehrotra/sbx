// Package osbtest is a fake OpenSandbox server - lifecycle API and execd - for tests of
// anything that speaks the contract. It is in-memory and needs no docker: a sandbox is a map
// entry, a file is a map entry, and a command is a tiny interpreter that understands just
// enough shell to exercise every event type.
//
// It follows the specs' shapes (field names, status codes, both SSE framings), because a fake
// that accepts what the real server would refuse is how a client ends up green in tests and
// broken against the real thing.
package osbtest

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is the fake. Its lifecycle API is at URL (with /v1 under it) and every sandbox's
// execd is on a second listener, under /execd/<id>.
type Server struct {
	// URL is the lifecycle server's root, without /v1.
	URL string

	// Key, when not empty, is the OPEN-SANDBOX-API-KEY every lifecycle call must carry.
	Key string

	// PendingPolls is how many endpoint lookups a new sandbox answers 404 to before it is
	// Running, so readiness polling has something to wait for.
	PendingPolls int

	// StandardSSE makes the command stream use `data:` framing. The default is the bare
	// JSON-then-blank-line framing that execd actually writes.
	StandardSSE bool

	lifecycle *httptest.Server
	execd     *httptest.Server

	mu        sync.Mutex
	seq       int
	sandboxes map[string]*sandbox
	requests  []Request
	running   map[string]chan struct{} // command id -> closed on interrupt
}

// Request is one call the fake received, for assertions.
type Request struct {
	Method, Path string
	Query        url.Values
	Header       http.Header
	Body         []byte
}

type sandbox struct {
	info    map[string]any
	token   string
	pending int
	files   map[string]*file
	dirs    map[string]int
}

type file struct {
	data  []byte
	mode  int
	owner string
	group string
	mtime time.Time
}

// New starts a fake. Close it when done.
func New() *Server {
	s := &Server{sandboxes: map[string]*sandbox{}, running: map[string]chan struct{}{}}
	s.lifecycle = httptest.NewServer(http.HandlerFunc(s.serveLifecycle))
	s.execd = httptest.NewServer(http.HandlerFunc(s.serveExecd))
	s.URL = s.lifecycle.URL

	return s
}

// Close stops both listeners.
func (s *Server) Close() {
	s.mu.Lock()
	for id, ch := range s.running {
		close(ch)
		delete(s.running, id)
	}
	s.mu.Unlock()

	s.lifecycle.CloseClientConnections()
	s.execd.CloseClientConnections()
	s.lifecycle.Close()
	s.execd.Close()
}

// Requests returns what the fake has received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Request(nil), s.requests...)
}

// Last returns the most recent request whose path ends with suffix and whose method matches.
func (s *Server) Last(method, suffix string) (Request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.requests) - 1; i >= 0; i-- {
		r := s.requests[i]
		if r.Method == method && strings.HasSuffix(r.Path, suffix) {
			return r, true
		}
	}

	return Request{}, false
}

// AddSandbox puts a Running sandbox in place without a create call, and returns its id.
func (s *Server) AddSandbox(image string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.newSandbox(map[string]any{"image": map[string]any{"uri": image}}, 0)
}

// File returns a file's content inside a sandbox, for assertions.
func (s *Server) File(id, p string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sb := s.sandboxes[id]
	if sb == nil {
		return "", false
	}

	f := sb.files[p]
	if f == nil {
		return "", false
	}

	return string(f.data), true
}

// PutFile writes a file inside a sandbox.
func (s *Server) PutFile(id, p, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sb := s.sandboxes[id]; sb != nil {
		sb.files[p] = &file{data: []byte(content), mode: 644, owner: "root", group: "root", mtime: time.Now()}
	}
}

// Dir reports whether a directory exists inside a sandbox, and its mode.
func (s *Server) Dir(id, p string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sb := s.sandboxes[id]
	if sb == nil {
		return 0, false
	}

	m, ok := sb.dirs[p]

	return m, ok
}

// Exists reports whether a sandbox is known and not terminated.
func (s *Server) Exists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.sandboxes[id]

	return ok
}

// Running reports how many foreground commands are blocked in the fake right now.
func (s *Server) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.running)
}

func (s *Server) record(r *http.Request) []byte {
	body, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	s.requests = append(s.requests, Request{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body,
	})
	s.mu.Unlock()

	return body
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}

// newSandbox must be called with mu held.
func (s *Server) newSandbox(req map[string]any, pending int) string {
	s.seq++
	id := fmt.Sprintf("osb-%012x", s.seq)
	now := time.Now().UTC().Truncate(time.Second)

	state := "Running"
	if pending > 0 {
		state = "Pending"
	}

	info := map[string]any{
		"id":         id,
		"status":     map[string]any{"state": state, "lastTransitionAt": now.Format(time.RFC3339)},
		"createdAt":  now.Format(time.RFC3339),
		"entrypoint": []string{"tail", "-f", "/dev/null"},
		"metadata":   map[string]string{},
	}

	for _, k := range []string{"image", "metadata", "entrypoint", "extensions", "platform"} {
		if v, ok := req[k]; ok && v != nil {
			info[k] = v
		}
	}

	if img, ok := info["image"].(map[string]any); ok {
		delete(img, "auth") // a server never echoes credentials back
	}

	if t, ok := req["timeout"].(float64); ok {
		info["expiresAt"] = now.Add(time.Duration(t) * time.Second).Format(time.RFC3339)
	}

	s.sandboxes[id] = &sandbox{
		info: info, token: "tok-" + id, pending: pending,
		files: map[string]*file{}, dirs: map[string]int{"/": 755, "/tmp": 777},
	}

	return id
}

func (s *Server) serveLifecycle(w http.ResponseWriter, r *http.Request) {
	body := s.record(r)

	if s.Key != "" && r.Header.Get("OPEN-SANDBOX-API-KEY") != s.Key {
		writeErr(w, 401, "UNAUTHORIZED", "missing or wrong OPEN-SANDBOX-API-KEY")

		return
	}

	p := strings.TrimPrefix(r.URL.Path, "/v1")
	if p == r.URL.Path {
		writeErr(w, 404, "NOT_FOUND", "the lifecycle API is under /v1")

		return
	}

	parts := strings.Split(strings.Trim(p, "/"), "/")

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case p == "/sandboxes" && r.Method == "POST":
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, 400, "INVALID_REQUEST_BODY", err.Error())

			return
		}

		img, _ := req["image"].(map[string]any)
		if img == nil || img["uri"] == "" {
			writeErr(w, 400, "INVALID_REQUEST_BODY", "image.uri is required")

			return
		}

		if ep, _ := req["entrypoint"].([]any); len(ep) == 0 {
			writeErr(w, 400, "INVALID_REQUEST_BODY", "entrypoint is required when image is given")

			return
		}

		if t, ok := req["timeout"].(float64); ok && t < 60 {
			writeErr(w, 400, "INVALID_REQUEST_BODY", "timeout must be at least 60 seconds")

			return
		}

		id := s.newSandbox(req, s.PendingPolls)
		writeJSON(w, 202, s.sandboxes[id].info)

	case p == "/sandboxes" && r.Method == "GET":
		s.list(w, r)

	case len(parts) >= 2 && parts[0] == "sandboxes":
		sb := s.sandboxes[parts[1]]
		if sb == nil {
			writeErr(w, 404, "SANDBOX_NOT_FOUND", "no sandbox "+parts[1])

			return
		}

		s.sandboxCall(w, r, sb, parts[2:], body)

	default:
		writeErr(w, 404, "NOT_FOUND", "no route "+r.Method+" "+r.URL.Path)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	states := q["state"]

	var want map[string]string

	if m := q.Get("metadata"); m != "" {
		want = map[string]string{}

		for _, kv := range strings.Split(m, "&") {
			k, v, _ := strings.Cut(kv, "=")
			k, _ = url.QueryUnescape(k)
			v, _ = url.QueryUnescape(v)
			want[k] = v
		}
	}

	ids := make([]string, 0, len(s.sandboxes))
	for id := range s.sandboxes {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	items := []any{}

	for _, id := range ids {
		info := s.sandboxes[id].info
		state := info["status"].(map[string]any)["state"].(string)

		if len(states) > 0 && !contains(states, state) {
			continue
		}

		md := toStrings(info["metadata"])

		ok := true

		for k, v := range want {
			if md[k] != v {
				ok = false
			}
		}

		if ok {
			items = append(items, info)
		}
	}

	page, size := atoiOr(q.Get("page"), 1), atoiOr(q.Get("pageSize"), 20)
	total := len(items)
	start, end := min((page-1)*size, total), min(page*size, total)
	pages := (total + size - 1) / size

	writeJSON(w, 200, map[string]any{
		"items": items[start:end],
		"pagination": map[string]any{
			"page": page, "pageSize": size, "totalItems": total, "totalPages": pages,
			"hasNextPage": page < pages,
		},
	})
}

func (s *Server) sandboxCall(w http.ResponseWriter, r *http.Request, sb *sandbox, rest []string, body []byte) {
	id := sb.info["id"].(string)

	switch {
	case len(rest) == 0 && r.Method == "GET":
		writeJSON(w, 200, sb.info)

	case len(rest) == 0 && r.Method == "DELETE":
		delete(s.sandboxes, id)
		w.WriteHeader(204)

	case len(rest) == 1 && rest[0] == "renew-expiration" && r.Method == "POST":
		var req struct {
			ExpiresAt time.Time `json:"expiresAt"`
		}

		if err := json.Unmarshal(body, &req); err != nil || req.ExpiresAt.IsZero() {
			writeErr(w, 400, "INVALID_REQUEST_BODY", "expiresAt is required")

			return
		}

		sb.info["expiresAt"] = req.ExpiresAt.UTC().Format(time.RFC3339Nano)
		writeJSON(w, 200, map[string]any{"expiresAt": sb.info["expiresAt"]})

	case len(rest) == 2 && rest[0] == "endpoints" && r.Method == "GET":
		if sb.pending > 0 {
			sb.pending--
			if sb.pending == 0 {
				sb.info["status"].(map[string]any)["state"] = "Running"
			}

			writeErr(w, 404, "ENDPOINT_NOT_READY", "sandbox is still Pending")

			return
		}

		port, _ := strconv.Atoi(rest[1])
		host := strings.TrimPrefix(s.execd.URL, "http://")

		if port != 44772 {
			writeJSON(w, 200, map[string]any{"endpoint": fmt.Sprintf("%s/execd/%s/proxy/%d", host, id, port)})

			return
		}

		// No scheme, as sbx and upstream's docker runtime both answer: the client prefixes
		// the lifecycle server's protocol.
		writeJSON(w, 200, map[string]any{
			"endpoint": host + "/execd/" + id,
			"headers":  map[string]string{"X-EXECD-ACCESS-TOKEN": sb.token},
		})

	case len(rest) == 1 && (rest[0] == "pause" || rest[0] == "resume") && r.Method == "POST":
		state := map[string]string{"pause": "Paused", "resume": "Running"}[rest[0]]
		sb.info["status"].(map[string]any)["state"] = state
		w.WriteHeader(202)

	default:
		writeErr(w, 404, "NOT_FOUND", "no route "+r.Method+" "+r.URL.Path)
	}
}

func (s *Server) serveExecd(w http.ResponseWriter, r *http.Request) {
	body := s.record(r)

	rest := strings.TrimPrefix(r.URL.Path, "/execd/")
	id, op, _ := strings.Cut(rest, "/")
	op = "/" + op

	s.mu.Lock()
	sb := s.sandboxes[id]
	s.mu.Unlock()

	if sb == nil {
		http.Error(w, "no such sandbox", 404)

		return
	}

	if r.Header.Get("X-EXECD-ACCESS-TOKEN") != sb.token {
		writeErr(w, 401, "UNAUTHORIZED", "missing or wrong X-EXECD-ACCESS-TOKEN")

		return
	}

	q := r.URL.Query()

	switch {
	case op == "/ping":
		w.WriteHeader(200)

	case op == "/metrics":
		writeJSON(w, 200, map[string]any{
			"cpu_count": 2.0, "cpu_used_pct": 12.5, "mem_total_mib": 2048.0, "mem_used_mib": 256.0,
			"timestamp": int64(1700000000000),
		})

	case op == "/command" && r.Method == "POST":
		s.command(w, r, body)

	case op == "/command" && r.Method == "DELETE":
		s.mu.Lock()
		ch, ok := s.running[q.Get("id")]
		if ok {
			close(ch)
			delete(s.running, q.Get("id"))
		}
		s.mu.Unlock()

		if !ok {
			writeErr(w, 404, "NOT_FOUND", "no running command "+q.Get("id"))

			return
		}

		w.WriteHeader(200)

	default:
		s.mu.Lock()
		defer s.mu.Unlock()

		s.files(w, r, sb, op, q, body)
	}
}

// files handles the filesystem group. Called with mu held.
func (s *Server) files(w http.ResponseWriter, r *http.Request, sb *sandbox, op string, q url.Values, body []byte) {
	switch {
	case op == "/files/upload" && r.Method == "POST":
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			writeErr(w, 400, "INVALID_REQUEST_BODY", err.Error())

			return
		}

		mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])

		var meta struct {
			Path  string `json:"path"`
			Owner string `json:"owner"`
			Group string `json:"group"`
			Mode  int    `json:"mode"`
		}

		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}

			data, _ := io.ReadAll(part)

			switch part.FormName() {
			case "metadata":
				_ = json.Unmarshal(data, &meta)
			case "file":
				if meta.Path == "" {
					writeErr(w, 400, "INVALID_REQUEST_BODY", "file part without metadata before it")

					return
				}

				sb.files[meta.Path] = &file{data: data, mode: meta.Mode, owner: or(meta.Owner, "root"), group: or(meta.Group, "root"), mtime: time.Now()}
			}
		}

		w.WriteHeader(200)

	case op == "/files/download" && r.Method == "GET":
		f := sb.files[q.Get("path")]
		if f == nil {
			writeErr(w, 404, "FILE_NOT_FOUND", "no such file "+q.Get("path"))

			return
		}

		data := f.data

		if rg := r.Header.Get("Range"); rg != "" {
			var a, b int
			if _, err := fmt.Sscanf(rg, "bytes=%d-%d", &a, &b); err == nil && a <= b && a < len(data) {
				data = data[a:min(b+1, len(data))]
				w.WriteHeader(206)
			}
		}

		_, _ = w.Write(data)

	case op == "/files" && r.Method == "DELETE":
		for _, p := range q["path"] {
			if sb.files[p] == nil {
				writeErr(w, 404, "FILE_NOT_FOUND", "no such file "+p)

				return
			}

			delete(sb.files, p)
		}

	case op == "/files/info" && r.Method == "GET":
		out := map[string]any{}
		for _, p := range q["path"] {
			if f := sb.files[p]; f != nil {
				out[p] = info(p, f)
			}
		}

		writeJSON(w, 200, out)

	case op == "/files/search" && r.Method == "GET":
		out := []any{}

		keys := make([]string, 0, len(sb.files))
		for p := range sb.files {
			keys = append(keys, p)
		}

		sort.Strings(keys)

		for _, p := range keys {
			if !strings.HasPrefix(p, strings.TrimRight(q.Get("path"), "/")+"/") {
				continue
			}

			if ok, _ := path.Match(or(q.Get("pattern"), "*"), path.Base(p)); ok {
				out = append(out, info(p, sb.files[p]))
			}
		}

		writeJSON(w, 200, out)

	case op == "/files/mv" && r.Method == "POST":
		var moves []struct{ Src, Dest string }
		if err := json.Unmarshal(body, &moves); err != nil {
			writeErr(w, 400, "INVALID_REQUEST_BODY", err.Error())

			return
		}

		for _, m := range moves {
			f := sb.files[m.Src]
			if f == nil {
				writeErr(w, 404, "FILE_NOT_FOUND", "no such file "+m.Src)

				return
			}

			delete(sb.files, m.Src)
			sb.files[m.Dest] = f
		}

	case op == "/files/replace" && r.Method == "POST":
		var edits map[string]struct{ Old, New string }
		if err := json.Unmarshal(body, &edits); err != nil {
			writeErr(w, 400, "INVALID_REQUEST_BODY", err.Error())

			return
		}

		out := map[string]any{}

		for p, e := range edits {
			f := sb.files[p]
			if f == nil {
				writeErr(w, 404, "FILE_NOT_FOUND", "no such file "+p)

				return
			}

			n := strings.Count(string(f.data), e.Old)
			f.data = []byte(strings.ReplaceAll(string(f.data), e.Old, e.New))
			out[p] = map[string]int{"replacedCount": n}
		}

		if q.Get("verbose") == "true" {
			writeJSON(w, 200, out)
		}

	case op == "/directories" && r.Method == "POST":
		var dirs map[string]struct{ Mode int }
		if err := json.Unmarshal(body, &dirs); err != nil {
			writeErr(w, 400, "INVALID_REQUEST_BODY", err.Error())

			return
		}

		for p, d := range dirs {
			sb.dirs[p] = d.Mode
		}

	case op == "/directories" && r.Method == "DELETE":
		for _, p := range q["path"] {
			delete(sb.dirs, p)

			for f := range sb.files {
				if strings.HasPrefix(f, strings.TrimRight(p, "/")+"/") {
					delete(sb.files, f)
				}
			}
		}

	default:
		writeErr(w, 404, "NOT_FOUND", "no route "+r.Method+" "+op)
	}
}

func info(p string, f *file) map[string]any {
	return map[string]any{
		"path": p, "type": "file", "size": len(f.data), "mode": f.mode, "owner": f.owner, "group": f.group,
		"modified_at": f.mtime.UTC().Format(time.RFC3339), "created_at": f.mtime.UTC().Format(time.RFC3339),
	}
}

// command runs the fake interpreter. It understands, separated by `;`:
//
//	echo TEXT    a stdout line       warn TEXT   a stderr line
//	exit N       fail with code N    sleep       block until interrupted or disconnected
//	pwd          print the cwd       env NAME    print an env var from the request
func (s *Server) command(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Command    string            `json:"command"`
		Argv       []string          `json:"argv"`
		Cwd        string            `json:"cwd"`
		Background bool              `json:"background"`
		Envs       map[string]string `json:"envs"`
	}

	if err := json.Unmarshal(body, &req); err != nil || (req.Command == "") == (len(req.Argv) == 0) {
		writeErr(w, 400, "INVALID_REQUEST_BODY", "exactly one of command or argv is required")

		return
	}

	s.mu.Lock()
	s.seq++
	cmdID := fmt.Sprintf("cmd-%d", s.seq)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)

	fl, _ := w.(http.Flusher)
	start := time.Now()
	ts := func() int64 { return time.Now().UnixMilli() }

	emit := func(ev map[string]any) {
		data, _ := json.Marshal(ev)
		if s.StandardSSE {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], data)
		} else {
			fmt.Fprintf(w, "%s\n\n", data)
		}

		if fl != nil {
			fl.Flush()
		}
	}

	emit(map[string]any{"type": "init", "text": cmdID, "timestamp": ts()})

	if req.Background {
		emit(map[string]any{"type": "execution_complete", "timestamp": ts(), "execution_time": 0})
		// Real execd holds the connection open after this for a grace period; a client
		// that waits for EOF instead of stopping at execution_complete hangs here.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}

		return
	}

	if s.StandardSSE {
		// A keepalive comment, which a parser must skip.
		fmt.Fprint(w, ": keepalive\n\n")
	}

	for _, stmt := range strings.Split(req.Command, ";") {
		verb, arg, _ := strings.Cut(strings.TrimSpace(stmt), " ")

		switch verb {
		case "echo":
			emit(map[string]any{"type": "stdout", "text": arg, "timestamp": ts()})
		case "warn":
			emit(map[string]any{"type": "stderr", "text": arg, "timestamp": ts()})
		case "pwd":
			emit(map[string]any{"type": "stdout", "text": or(req.Cwd, "/"), "timestamp": ts()})
		case "env":
			emit(map[string]any{"type": "stdout", "text": req.Envs[arg], "timestamp": ts()})
		case "exit":
			emit(map[string]any{"type": "error", "timestamp": ts(), "error": map[string]any{
				"ename": "CommandExecError", "evalue": arg, "traceback": []string{"exit status " + arg},
			}})

			return
		case "sleep":
			ch := make(chan struct{})

			s.mu.Lock()
			s.running[cmdID] = ch
			s.mu.Unlock()

			select {
			case <-ch:
				emit(map[string]any{"type": "error", "timestamp": ts(), "error": map[string]any{
					"ename": "CommandExecError", "evalue": "130", "traceback": []string{"interrupted"},
				}})

				return
			case <-r.Context().Done():
				s.mu.Lock()
				delete(s.running, cmdID)
				s.mu.Unlock()

				return
			}
		}
	}

	emit(map[string]any{"type": "execution_complete", "timestamp": ts(), "execution_time": time.Since(start).Milliseconds()})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}

	return false
}

func toStrings(v any) map[string]string {
	out := map[string]string{}

	switch m := v.(type) {
	case map[string]string:
		return m
	case map[string]any:
		for k, x := range m {
			out[k], _ = x.(string)
		}
	}

	return out
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}

	return def
}

func or(s, def string) string {
	if s == "" {
		return def
	}

	return s
}
