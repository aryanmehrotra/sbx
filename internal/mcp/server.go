// Package mcp is a Model Context Protocol server over stdio: JSON-RPC 2.0, one message per
// line, requests in on stdin and answers out on stdout.
//
// It is written against the protocol, not against a framework, because the root module takes
// no dependencies and because the protocol is small: a handshake, a tool list, a tool call,
// a ping, and a cancellation notice. What it gets right that is easy to get wrong:
//
//   - stdout carries protocol and nothing else. One stray log line there and the client drops
//     the connection with a parse error, so everything diagnostic goes to the log writer.
//   - a tool that fails answers with a result whose isError is true, not with a JSON-RPC
//     error. The model reads the first and can correct itself; the second is for a client
//     that asked for something the protocol does not have.
//   - requests are handled concurrently. A command that runs for ten minutes must not stop
//     the client from pinging, listing tools, or cancelling that very command.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
)

// Protocol versions this server speaks, newest first. The newest is offered to a client that
// asks for one it does not know, which is what the spec's negotiation says to do.
var ProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// structuredSince is the first version with structuredContent in a tool result. An older
// client would not expect the field; it is harmless but it is also noise.
const structuredSince = "2025-06-18"

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// Tool is one callable tool.
type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]any
	Annotations *Annotations
	// Handler does the work. Its result is serialized to JSON for the text content, and
	// becomes structuredContent (wrapped as {"result": ...} when it is not an object). A
	// returned error becomes a result with isError true.
	Handler func(ctx context.Context, call *Call) (any, error)
}

// Annotations are hints to the client about what a tool does.
type Annotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// Call is one tools/call.
type Call struct {
	Arguments json.RawMessage
	progress  func(done, total float64, msg string)
}

// Progress reports progress to the client, if it asked for it with a progress token.
func (c *Call) Progress(done, total float64, msg string) {
	if c.progress != nil {
		c.progress(done, total, msg)
	}
}

// Bind decodes the arguments into v, rejecting unknown fields so a misspelt argument is an
// error the model sees rather than a silently ignored option.
func (c *Call) Bind(v any) error {
	args := c.Arguments
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = []byte("{}")
	}

	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()

	if err := d.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}

	return nil
}

// Server is an MCP server. Register tools, then Serve.
type Server struct {
	Name, Title, Version string
	Instructions         string
	// Log receives diagnostics. Never stdout.
	Log io.Writer

	tools map[string]Tool
}

// NewServer returns a server with no tools.
func NewServer(name, version string) *Server {
	return &Server{Name: name, Version: version, Log: io.Discard, tools: map[string]Tool{}}
}

// Register adds tools. A second tool with the same name replaces the first.
func (s *Server) Register(tools ...Tool) {
	for _, t := range tools {
		s.tools[t.Name] = t
	}
}

// Tools returns the registered tools, sorted by name.
func (s *Server) Tools() []Tool {
	out := make([]Tool, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// session is the state of one connection.
type session struct {
	s   *Server
	ctx context.Context

	wmu sync.Mutex
	w   *bufio.Writer

	mu       sync.Mutex
	version  string
	inflight map[string]context.CancelFunc
	wg       sync.WaitGroup
}

// Serve reads requests from r and writes answers to w until r ends or ctx is done. It returns
// nil on a clean end of input.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ss := &session{s: s, ctx: ctx, w: bufio.NewWriter(w), inflight: map[string]context.CancelFunc{}}

	lines := make(chan []byte)
	readErr := make(chan error, 1)

	go func() {
		br := bufio.NewReaderSize(r, 1<<20)

		for {
			// ReadBytes, not a Scanner: file_write carries a whole file in one line, and a
			// Scanner's token cap would turn a large write into a dropped connection.
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}

			if err != nil {
				if err == io.EOF {
					err = nil
				}

				readErr <- err

				return
			}
		}
	}()

	var err error

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case line := <-lines:
			ss.handleLine(line)
		case err = <-readErr:
			break loop
		}
	}

	// End of input is the stdio transport's shutdown signal, but requests already read are
	// still answered: `printf '...\n' | sbx mcp` should get its answers, and a client that
	// cannot wait has SIGTERM, which arrives as ctx. Either way nothing writes after return.
	if ctx.Err() != nil {
		ss.wg.Wait()

		return ctx.Err()
	}

	ss.wg.Wait()
	cancel()

	return err
}

func (ss *session) logf(format string, args ...any) {
	fmt.Fprintf(ss.s.Log, "sbx mcp: "+format+"\n", args...)
}

func (ss *session) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		ss.logf("cannot encode an answer: %v", err)

		data, _ = json.Marshal(response{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: codeInternal, Message: "the server could not encode its answer"}})
	}

	ss.wmu.Lock()
	defer ss.wmu.Unlock()

	_, _ = ss.w.Write(data)
	_ = ss.w.WriteByte('\n')

	if err := ss.w.Flush(); err != nil {
		ss.logf("writing to the client failed: %v", err)
	}
}

func (ss *session) handleLine(line []byte) {
	line = bytes.TrimSpace(line)

	// A batch: a JSON array of messages, answered with an array. 2025-03-26 allows it and
	// later versions removed it; answering one costs nothing and refusing one strands a client.
	if len(line) > 0 && line[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(line, &batch); err != nil {
			ss.write(errResp(nil, codeParse, "parse error: "+err.Error()))

			return
		}

		if len(batch) == 0 {
			ss.write(errResp(nil, codeInvalidRequest, "an empty batch is not a request"))

			return
		}

		var runs []func() *response

		for _, raw := range batch {
			if run := ss.prepare(raw); run != nil {
				runs = append(runs, run)
			}
		}

		if len(runs) == 0 {
			return
		}

		ss.wg.Add(1)

		go func() {
			defer ss.wg.Done()

			var out []any

			var mu sync.Mutex

			var wg sync.WaitGroup

			for _, run := range runs {
				wg.Add(1)

				go func(run func() *response) {
					defer wg.Done()

					if r := run(); r != nil {
						mu.Lock()
						out = append(out, r)
						mu.Unlock()
					}
				}(run)
			}

			wg.Wait()

			if len(out) > 0 {
				ss.write(out)
			}
		}()

		return
	}

	var probe message
	if err := json.Unmarshal(line, &probe); err != nil {
		ss.logf("unparseable input: %v", err)
		ss.write(errResp(nil, codeParse, "parse error: "+err.Error()))

		return
	}

	// Requests are answered from their own goroutine, so a slow tool never blocks the next
	// line - most of all the cancellation notice for that very tool.
	run := ss.prepare(line)
	if run == nil {
		return
	}

	ss.wg.Add(1)

	go func() {
		defer ss.wg.Done()

		if r := run(); r != nil {
			ss.write(r)
		}
	}()
}

func errResp(id json.RawMessage, code int, msg string) response {
	if id == nil {
		id = json.RawMessage("null")
	}

	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// prepare reads one message and returns the work of answering it, or nil when it needs no
// answer. It runs in input order on the reading goroutine, so a request is registered as in
// flight before the next line is read - otherwise a cancellation sent right behind it could
// arrive first, find nothing to cancel, and be lost. The returned func may run concurrently.
func (ss *session) prepare(raw json.RawMessage) func() *response {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		r := errResp(nil, codeInvalidRequest, "not a JSON-RPC message: "+err.Error())

		return answer(r)
	}

	isRequest := len(m.ID) > 0 && string(m.ID) != "null"

	if m.JSONRPC != "2.0" {
		if !isRequest {
			return nil
		}

		r := errResp(m.ID, codeInvalidRequest, `jsonrpc must be "2.0"`)

		return answer(r)
	}

	if m.Method == "" {
		// A response to a request of ours. This server sends none, so there is nothing to
		// match it against.
		if !isRequest && (m.Result != nil || m.Error != nil) {
			return nil
		}

		r := errResp(m.ID, codeInvalidRequest, "a request needs a method")

		return answer(r)
	}

	if !isRequest {
		ss.notification(m)

		return nil
	}

	ctx, cancel := context.WithCancel(ss.ctx)
	key := string(m.ID)

	ss.mu.Lock()
	ss.inflight[key] = cancel
	ss.mu.Unlock()

	return func() *response {
		defer func() {
			ss.mu.Lock()
			delete(ss.inflight, key)
			ss.mu.Unlock()
			cancel()
		}()

		result, rerr := ss.dispatch(ctx, m)

		// A cancelled request gets no answer: the spec says the client has stopped waiting, and
		// an answer to an id it has forgotten is at best noise.
		if ctx.Err() != nil && ss.ctx.Err() == nil {
			return nil
		}

		if rerr != nil {
			return &response{JSONRPC: "2.0", ID: m.ID, Error: rerr}
		}

		return &response{JSONRPC: "2.0", ID: m.ID, Result: result}
	}
}

func answer(r response) func() *response { return func() *response { return &r } }

func (ss *session) notification(m message) {
	switch m.Method {
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
			Reason    string          `json:"reason"`
		}

		if json.Unmarshal(m.Params, &p) != nil || len(p.RequestID) == 0 {
			return
		}

		ss.mu.Lock()
		cancel := ss.inflight[string(p.RequestID)]
		ss.mu.Unlock()

		if cancel != nil {
			cancel()
		}
	case "notifications/initialized":
	default:
		// Unknown notifications are ignored, as the spec requires.
	}
}

func (ss *session) dispatch(ctx context.Context, m message) (any, *rpcError) {
	switch m.Method {
	case "initialize":
		return ss.initialize(m.Params)
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return ss.listTools(), nil
	case "tools/call":
		return ss.callTool(ctx, m)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + m.Method}
	}
}

func (ss *session) initialize(params json.RawMessage) (any, *rpcError) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}

	if err := json.Unmarshal(params, &p); err != nil || p.ProtocolVersion == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "initialize needs params.protocolVersion"}
	}

	version := ProtocolVersions[0]

	for _, v := range ProtocolVersions {
		if v == p.ProtocolVersion {
			version = v

			break
		}
	}

	ss.mu.Lock()
	ss.version = version
	ss.mu.Unlock()

	info := map[string]any{"name": ss.s.Name, "version": ss.s.Version}
	if ss.s.Title != "" {
		info["title"] = ss.s.Title
	}

	out := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      info,
	}

	if ss.s.Instructions != "" {
		out["instructions"] = ss.s.Instructions
	}

	return out, nil
}

func (ss *session) negotiated() string {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	return ss.version
}

func (ss *session) listTools() any {
	tools := []map[string]any{}

	for _, t := range ss.s.Tools() {
		d := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
		if t.Title != "" {
			d["title"] = t.Title
		}

		if t.Annotations != nil {
			d["annotations"] = t.Annotations
		}

		tools = append(tools, d)
	}

	return map[string]any{"tools": tools}
}

func (ss *session) callTool(ctx context.Context, m message) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}

	if err := json.Unmarshal(m.Params, &p); err != nil || p.Name == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call needs params.name"}
	}

	t, ok := ss.s.tools[p.Name]
	if !ok {
		// The spec's own example answers an unknown tool with -32602: the client named
		// something that is not in tools/list, which is a protocol mistake, not a tool failure.
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name + "; tools/list has the names"}
	}

	call := &Call{Arguments: p.Arguments}

	if tok := p.Meta.ProgressToken; len(tok) > 0 && string(tok) != "null" {
		call.progress = func(done, total float64, msg string) {
			params := map[string]any{"progressToken": tok, "progress": done}
			if total > 0 {
				params["total"] = total
			}

			if msg != "" {
				params["message"] = msg
			}

			ss.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": params})
		}
	}

	res, err := safeCall(ctx, t, call)
	if err != nil {
		return toolError(err), nil
	}

	return ss.toolResult(res), nil
}

// safeCall runs a handler and turns a panic into a tool error. The panic would otherwise take
// the whole server down, and every other in-flight call with it, over one bad argument.
func safeCall(ctx context.Context, t Tool, call *Call) (res any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error in %s: %v", t.Name, r)
		}
	}()

	return t.Handler(ctx, call)
}

func toolError(err error) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": err.Error()}},
		"isError": true,
	}
}

func (ss *session) toolResult(res any) map[string]any {
	data, err := json.Marshal(res)
	if err != nil {
		return toolError(fmt.Errorf("the result could not be encoded: %w", err))
	}

	out := map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(data)}},
		"isError": false,
	}

	if ss.negotiated() >= structuredSince {
		// structuredContent must be an object. A list is wrapped the way upstream's Python
		// framework wraps one, so a client written against either reads the same key.
		var obj map[string]any
		if json.Unmarshal(data, &obj) == nil && obj != nil {
			out["structuredContent"] = json.RawMessage(data)
		} else {
			out["structuredContent"] = map[string]json.RawMessage{"result": data}
		}
	}

	return out
}
