package jupyter

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// EventType is the "type" field of one execd code-execution event.
type EventType string

// The event types of execd-api.yaml's ServerStreamEvent. Names and values are upstream's.
const (
	EventInit           EventType = "init"
	EventStatus         EventType = "status"
	EventError          EventType = "error"
	EventStdout         EventType = "stdout"
	EventStderr         EventType = "stderr"
	EventResult         EventType = "result"
	EventComplete       EventType = "execution_complete"
	EventExecutionCount EventType = "execution_count"
	EventPing           EventType = "ping"
)

// ErrorOutput is the error payload of an EventError, and of a Jupyter "error" message.
//
// Traceback has no omitempty on purpose: upstream serialises a missing traceback as null, and a
// client written against upstream may distinguish the two.
type ErrorOutput struct {
	EName     string   `json:"ename"`
	EValue    string   `json:"evalue"`
	Traceback []string `json:"traceback"`
}

// Event is one server-sent event, field for field the wire shape of upstream's
// ServerStreamEvent, so that encoding it is all execd has to do.
type Event struct {
	Type           EventType      `json:"type,omitempty"`
	Text           string         `json:"text,omitempty"`
	ExecutionCount int            `json:"execution_count,omitempty"`
	ExecutionTime  int64          `json:"execution_time,omitempty"`
	Timestamp      int64          `json:"timestamp,omitempty"`
	Results        map[string]any `json:"results,omitempty"`
	Error          *ErrorOutput   `json:"error,omitempty"`
}

// Encode returns the event as it goes on the wire: the JSON object followed by a blank line.
//
// Not "data: {...}". Upstream execd writes bare JSON blobs separated by blank lines, and its SDKs
// parse exactly that (the Go SDK's stream reader has an explicit branch for a line starting with
// '{'). Standard SSE framing would also parse in the Go SDK, but matching the reference byte for
// byte means no client has to have been written tolerantly.
func (e Event) Encode() []byte {
	b, _ := json.Marshal(e) // every field is a plain value or a JSON-decoded map; cannot fail

	return append(b, '\n', '\n')
}

// SSEHeaders are the response headers upstream sets on a code-execution stream.
// X-Accel-Buffering stops an nginx in front of the sandbox from holding the stream back.
var SSEHeaders = map[string]string{
	"Content-Type":      "text/event-stream",
	"Cache-Control":     "no-cache",
	"Connection":        "keep-alive",
	"X-Accel-Buffering": "no",
}

// EventWriter writes events to an HTTP response, setting the stream headers before the first
// one and flushing after each. It is safe for concurrent use.
//
// Headers go out with the first event rather than up front so that a handler can still answer
// with an ordinary JSON error if Run fails before anything has been emitted.
type EventWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	started bool
	err     error
}

// NewEventWriter wraps a response.
func NewEventWriter(w http.ResponseWriter) *EventWriter {
	return &EventWriter{w: w}
}

// Started reports whether any event has been written, i.e. whether the status line is gone.
func (ew *EventWriter) Started() bool {
	ew.mu.Lock()
	defer ew.mu.Unlock()

	return ew.started
}

// Write sends one event. Once a write fails - the client went away - later ones are dropped and
// the first error is returned each time, because retrying a dead response only spams the log.
func (ew *EventWriter) Write(e Event) error {
	ew.mu.Lock()
	defer ew.mu.Unlock()

	if ew.err != nil {
		return ew.err
	}

	if !ew.started {
		h := ew.w.Header()
		for k, v := range SSEHeaders {
			h.Set(k, v)
		}

		ew.started = true
	}

	if e.Timestamp == 0 {
		e.Timestamp = time.Now().UnixMilli()
	}

	p := e.Encode()

	n, err := ew.w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}

	if err != nil {
		ew.err = err

		return err
	}

	// Through ResponseController rather than a Flusher assertion: execd wraps the writer to
	// recover panics, and the wrapper offers Unwrap, not Flush.
	_ = http.NewResponseController(ew.w).Flush()

	return nil
}

// Emit adapts the writer to Run's emit callback, discarding the write error: a client that has
// gone away is noticed through the request context, which is what stops the run.
func (ew *EventWriter) Emit(e Event) { _ = ew.Write(e) }
