package osbclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strings"
	"time"
)

// Execd is a client for one sandbox's execd. Get one from Client.Execd or Client.WaitReady;
// it carries the access token the lifecycle server handed out.
type Execd struct {
	r *requester
}

// NewExecd returns a client for an execd at baseURL, for callers that already know where it
// is and what its token is (a test, or a sandbox reached without the lifecycle API).
func NewExecd(baseURL, token string, h *http.Client) *Execd {
	if h == nil {
		h = &http.Client{}
	}

	hdr := map[string]string{}
	if token != "" {
		hdr[ExecdTokenHeader] = token
	}

	return &Execd{r: &requester{
		base: strings.TrimRight(baseURL, "/"), headers: hdr, http: h,
		timeout: DefaultRequestTimeout, userAgent: "sbx-osbclient",
	}}
}

// BaseURL is the execd root this client calls.
func (e *Execd) BaseURL() string { return e.r.base }

// Ping succeeds when execd answers.
func (e *Execd) Ping(ctx context.Context) error {
	_, err := e.r.do(ctx, "GET", "/ping", nil, nil, nil)

	return err
}

// Metrics is execd's resource snapshot, in the contract's units.
type Metrics struct {
	CPUCount    float64 `json:"cpu_count"`
	CPUUsedPct  float64 `json:"cpu_used_pct"`
	MemTotalMiB float64 `json:"mem_total_mib"`
	MemUsedMiB  float64 `json:"mem_used_mib"`
	Timestamp   int64   `json:"timestamp"` // unix milliseconds
}

// Metrics reads the sandbox's current resource use.
func (e *Execd) Metrics(ctx context.Context) (*Metrics, error) {
	var m Metrics
	if _, err := e.r.do(ctx, "GET", "/metrics", nil, nil, &m); err != nil {
		return nil, err
	}

	return &m, nil
}

// CommandRequest is the body of POST /command. Set exactly one of Command (shell text) or
// Argv. Timeout is milliseconds; zero means none.
type CommandRequest struct {
	Command    string            `json:"command,omitempty"`
	Argv       []string          `json:"argv,omitempty"`
	Cwd        string            `json:"cwd,omitempty"`
	Background bool              `json:"background,omitempty"`
	Timeout    int64             `json:"timeout,omitempty"`
	UID        *int              `json:"uid,omitempty"`
	GID        *int              `json:"gid,omitempty"`
	Envs       map[string]string `json:"envs,omitempty"`
}

// Event is one server-sent event from a command stream.
type Event struct {
	Type           string         `json:"type"`
	Text           string         `json:"text,omitempty"`
	ExecutionCount *int           `json:"execution_count,omitempty"`
	ExecutionTime  int64          `json:"execution_time,omitempty"` // milliseconds
	Timestamp      int64          `json:"timestamp"`                // unix milliseconds
	Results        map[string]any `json:"results,omitempty"`
	Error          *EventError    `json:"error,omitempty"`
}

// EventError is the error payload of an "error" event. For a shell command that exited
// non-zero, Value holds the exit code as text - which is how the upstream SDKs infer it.
type EventError struct {
	Name      string   `json:"ename"`
	Value     string   `json:"evalue"`
	Traceback []string `json:"traceback,omitempty"`
}

// StreamCommand runs a command and calls fn for every event, in order, until the stream ends,
// fn returns an error, or ctx ends. A background command's stream ends at
// execution_complete: execd sends the stream terminator only after a grace sleep that a
// closed connection can lose, and waiting for it is how upstream's #1528 hung.
func (e *Execd) StreamCommand(ctx context.Context, req CommandRequest, fn func(Event) error) error {
	resp, err := e.r.send(ctx, "POST", "/command", nil, req, http.Header{"Accept": {"text/event-stream"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return readEvents(resp.Body, func(ev Event) error {
		if err := fn(ev); err != nil {
			return err
		}

		if req.Background && ev.Type == "execution_complete" {
			return errStop
		}

		return nil
	})
}

// RunCommand runs a command to completion and returns what it produced. For a background
// command it returns once execd has started it, with the Execution's ID set for Interrupt.
func (e *Execd) RunCommand(ctx context.Context, req CommandRequest) (*Execution, error) {
	x := NewExecution()

	err := e.StreamCommand(ctx, req, func(ev Event) error {
		x.Apply(ev)

		return nil
	})
	if err != nil {
		return x, err
	}

	if !req.Background {
		x.InferExitCode()
	}

	return x, nil
}

// InterruptCommand stops a running command by the ID its init event carried.
func (e *Execd) InterruptCommand(ctx context.Context, id string) error {
	_, err := e.r.do(ctx, "DELETE", "/command", url.Values{"id": {id}}, nil, nil)

	return err
}

// CommandStatus is GET /command/status/{id}.
type CommandStatus struct {
	ID         string     `json:"id"`
	Content    string     `json:"content,omitempty"`
	Running    bool       `json:"running"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// CommandStatus reads whether a command is still running and how it ended.
func (e *Execd) CommandStatus(ctx context.Context, id string) (*CommandStatus, error) {
	var s CommandStatus
	if _, err := e.r.do(ctx, "GET", "/command/status/"+url.PathEscape(id), nil, nil, &s); err != nil {
		return nil, err
	}

	return &s, nil
}

// FileInfo is execd's description of one file or directory. Mode is the permission bits
// written as octal digits read in decimal (755, not 493), as the contract has it.
type FileInfo struct {
	Path       string    `json:"path"`
	Type       string    `json:"type,omitempty"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	CreatedAt  time.Time `json:"created_at"`
	Owner      string    `json:"owner"`
	Group      string    `json:"group"`
	Mode       int       `json:"mode"`
}

// Permission is ownership and mode for a created directory or uploaded file. Empty Owner or
// Group leaves them as execd's default.
type Permission struct {
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
	Mode  int    `json:"mode"`
}

// Stat reads metadata for each path.
func (e *Execd) Stat(ctx context.Context, paths ...string) (map[string]FileInfo, error) {
	out := map[string]FileInfo{}
	if _, err := e.r.do(ctx, "GET", "/files/info", url.Values{"path": paths}, nil, &out); err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteFiles removes files.
func (e *Execd) DeleteFiles(ctx context.Context, paths ...string) error {
	_, err := e.r.do(ctx, "DELETE", "/files", url.Values{"path": paths}, nil, nil)

	return err
}

// Move is one rename.
type Move struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

// MoveFiles renames files or directories, in order.
func (e *Execd) MoveFiles(ctx context.Context, moves ...Move) error {
	_, err := e.r.do(ctx, "POST", "/files/mv", nil, moves, nil)

	return err
}

// SearchFiles lists files under dir whose names match a glob pattern.
func (e *Execd) SearchFiles(ctx context.Context, dir, pattern string) ([]FileInfo, error) {
	q := url.Values{"path": {dir}}
	if pattern != "" {
		q.Set("pattern", pattern)
	}

	var out []FileInfo
	if _, err := e.r.do(ctx, "GET", "/files/search", q, nil, &out); err != nil {
		return nil, err
	}

	if out == nil {
		out = []FileInfo{}
	}

	return out, nil
}

// Replacement is one file's find-and-replace: every occurrence of Old becomes New.
type Replacement struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// ReplaceInFiles replaces text in each file (keyed by path) and reports how many
// occurrences each file had. It asks for the verbose answer, which older execds ignore - a
// file missing from the answer is reported as -1, "not counted", rather than as zero.
func (e *Execd) ReplaceInFiles(ctx context.Context, edits map[string]Replacement) (map[string]int, error) {
	var out map[string]struct {
		ReplacedCount int `json:"replacedCount"`
	}

	if _, err := e.r.do(ctx, "POST", "/files/replace", url.Values{"verbose": {"true"}}, edits, &out); err != nil {
		return nil, err
	}

	counts := make(map[string]int, len(edits))
	for p := range edits {
		if r, ok := out[p]; ok {
			counts[p] = r.ReplacedCount
		} else {
			counts[p] = -1
		}
	}

	return counts, nil
}

// WriteFile uploads data to p inside the sandbox, creating or replacing it.
func (e *Execd) WriteFile(ctx context.Context, p string, data []byte, perm Permission) error {
	var body bytes.Buffer

	mw := multipart.NewWriter(&body)

	meta, err := json.Marshal(struct {
		Path string `json:"path"`
		Permission
	}{p, perm})
	if err != nil {
		return err
	}

	// Two parts per file, metadata first: execd reads them in sequence and pairs each file
	// part with the metadata before it.
	mh := textproto.MIMEHeader{}
	mh.Set("Content-Disposition", `form-data; name="metadata"; filename="metadata"`)
	mh.Set("Content-Type", "application/json")

	pw, err := mw.CreatePart(mh)
	if err != nil {
		return err
	}

	_, _ = pw.Write(meta)

	fh := textproto.MIMEHeader{}
	fh.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName(p)))
	fh.Set("Content-Type", "application/octet-stream")

	fw, err := mw.CreatePart(fh)
	if err != nil {
		return err
	}

	_, _ = fw.Write(data)

	if err := mw.Close(); err != nil {
		return err
	}

	ctx, cancel := e.r.bound(ctx)
	defer cancel()

	resp, err := e.r.send(ctx, "POST", "/files/upload", nil, &body, http.Header{"Content-Type": {mw.FormDataContentType()}})
	if err != nil {
		return err
	}

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.Body.Close()
}

func fileName(p string) string {
	b := path.Base(p)
	if b == "/" || b == "." || b == "" {
		return "file"
	}

	return b
}

// ReadFile downloads a file. rangeHeader, when set, is sent as the HTTP Range header
// ("bytes=0-1023") and the answer is only that slice.
func (e *Execd) ReadFile(ctx context.Context, p, rangeHeader string) ([]byte, error) {
	var hdr http.Header
	if rangeHeader != "" {
		hdr = http.Header{"Range": {rangeHeader}}
	}

	ctx, cancel := e.r.bound(ctx)
	defer cancel()

	resp, err := e.r.send(ctx, "GET", "/files/download", url.Values{"path": {p}}, nil, hdr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// MakeDirs creates each directory (keyed by path) and its parents, like mkdir -p.
func (e *Execd) MakeDirs(ctx context.Context, dirs map[string]Permission) error {
	_, err := e.r.do(ctx, "POST", "/directories", nil, dirs, nil)

	return err
}

// DeleteDirs removes directories and everything in them, like rm -rf.
func (e *Execd) DeleteDirs(ctx context.Context, paths ...string) error {
	_, err := e.r.do(ctx, "DELETE", "/directories", url.Values{"path": paths}, nil, nil)

	return err
}

// bound applies the request timeout to a call that reads its own body.
func (r *requester) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.timeout > 0 {
		return context.WithTimeout(ctx, r.timeout)
	}

	return ctx, func() {}
}
