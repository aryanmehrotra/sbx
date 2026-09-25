//go:build unix

package execd

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Event types, as ServerStreamEvent.type in the spec.
const (
	evInit     = "init"
	evStdout   = "stdout"
	evStderr   = "stderr"
	evError    = "error"
	evComplete = "execution_complete"
	evPing     = "ping"
)

// event is one ServerStreamEvent. Every field is omitempty because upstream's is: a client
// written against upstream has only ever seen the fields an event actually carries.
type event struct {
	Type          string      `json:"type,omitempty"`
	Text          string      `json:"text,omitempty"`
	ExecutionTime int64       `json:"execution_time,omitempty"`
	Timestamp     int64       `json:"timestamp,omitempty"`
	Error         *eventError `json:"error,omitempty"`
}

type eventError struct {
	EName     string   `json:"ename,omitempty"`
	EValue    string   `json:"evalue,omitempty"`
	Traceback []string `json:"traceback,omitempty"`
}

// pingInterval is upstream's keep-alive cadence. It keeps a proxy between client and execd
// from timing out a command that prints nothing for a while.
var pingInterval = 3 * time.Second

// stream writes upstream's SSE framing: each event is a bare JSON object followed by a blank
// line. Not "data: {...}" - upstream's execd has never sent the field prefix, and every
// OpenSandbox SDK parses a line starting with '{' as a whole event for exactly that reason.
//
// Headers are committed on the first event, not before, so a handler that fails before
// anything has happened can still answer with a status code and a JSON error body.
type stream struct {
	w  http.ResponseWriter
	rc *http.ResponseController

	mu      sync.Mutex
	started bool
	broken  bool

	stopPing chan struct{}
	pinging  sync.WaitGroup
}

func newStream(w http.ResponseWriter) *stream {
	return &stream{w: w, rc: http.NewResponseController(w), stopPing: make(chan struct{})}
}

func (s *stream) send(ev event) {
	if ev.Timestamp == 0 {
		ev.Timestamp = time.Now().UnixMilli()
	}

	b, err := json.Marshal(ev)
	if err != nil {
		return
	}

	b = append(b, '\n', '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		// Tells nginx-style proxies not to buffer, which would hold every event until the
		// command finished and defeat the point of streaming.
		h.Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}

	// Once a write fails the client is gone. Keep accepting events so the caller's logic does
	// not have to care, but stop touching a dead connection.
	if s.broken {
		return
	}

	if _, err := s.w.Write(b); err != nil {
		s.broken = true
		return
	}

	_ = s.rc.Flush()
}

func (s *stream) hasStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.started
}

// startPing sends a ping event every pingInterval until stop is called.
func (s *stream) startPing() {
	s.pinging.Add(1)

	go func() {
		defer s.pinging.Done()

		t := time.NewTicker(pingInterval)
		defer t.Stop()

		for {
			select {
			case <-s.stopPing:
				return
			case <-t.C:
				s.send(event{Type: evPing, Text: "pong"})
			}
		}
	}()
}

// stop ends the ping goroutine and waits for it, so nothing writes to the ResponseWriter after
// the handler has returned - net/http forbids that, and the race detector notices.
func (s *stream) stop() {
	select {
	case <-s.stopPing:
	default:
		close(s.stopPing)
	}

	s.pinging.Wait()
}

func errorEvent(name, value string, traceback ...string) event {
	return event{Type: evError, Error: &eventError{EName: name, EValue: value, Traceback: traceback}}
}
