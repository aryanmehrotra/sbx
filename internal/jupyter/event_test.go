package jupyter

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The encodings are compared as bytes because the wire shape is the contract: field names,
// omitted zero values, and the bare-JSON-plus-blank-line framing upstream's SDKs parse.
func TestEventEncoding(t *testing.T) {
	cases := []struct {
		ev   Event
		want string
	}{
		{Event{Type: EventInit, Text: "abc", Timestamp: 1}, `{"type":"init","text":"abc","timestamp":1}`},
		{Event{Type: EventExecutionCount, ExecutionCount: 3, Timestamp: 1}, `{"type":"execution_count","execution_count":3,"timestamp":1}`},
		{Event{Type: EventResult, Results: map[string]any{"text": "4"}, Timestamp: 1}, `{"type":"result","timestamp":1,"results":{"text":"4"}}`},
		{Event{Type: EventComplete, ExecutionTime: 150, Timestamp: 1}, `{"type":"execution_complete","execution_time":150,"timestamp":1}`},
		// traceback is null, not absent, when there is none - as upstream sends it.
		{Event{Type: EventError, Error: &ErrorOutput{EName: "ContextCancelled", EValue: "Interrupt kernel"}, Timestamp: 1},
			`{"type":"error","timestamp":1,"error":{"ename":"ContextCancelled","evalue":"Interrupt kernel","traceback":null}}`},
		{Event{Type: EventPing, Text: "pong", Timestamp: 1}, `{"type":"ping","text":"pong","timestamp":1}`},
	}

	for _, tc := range cases {
		if got := string(tc.ev.Encode()); got != tc.want+"\n\n" {
			t.Errorf("Encode(%s)\n got %q\nwant %q", tc.ev.Type, got, tc.want+"\n\n")
		}
	}
}

func TestEventWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	ew := NewEventWriter(rec)

	if ew.Started() || rec.Header().Get("Content-Type") != "" {
		t.Fatal("headers set before any event: a pre-stream error could no longer be JSON")
	}

	if err := ew.Write(Event{Type: EventInit, Text: "x"}); err != nil {
		t.Fatal(err)
	}

	ew.Emit(Event{Type: EventStdout, Text: "hi\n", Timestamp: 7})

	for k, v := range SSEHeaders {
		if rec.Header().Get(k) != v {
			t.Errorf("header %s = %q", k, rec.Header().Get(k))
		}
	}

	if !rec.Flushed || !ew.Started() {
		t.Fatal("not flushed")
	}

	body := rec.Body.String()
	if want := `{"type":"stdout","text":"hi\n","timestamp":7}` + "\n\n"; body[len(body)-len(want):] != want {
		t.Fatalf("body = %q", body)
	}

	// A timestamp is filled in when the caller left it zero.
	if body[:len(`{"type":"init","text":"x","timestamp":`)] != `{"type":"init","text":"x","timestamp":` {
		t.Fatalf("body = %q", body)
	}
}

type failingWriter struct {
	http.ResponseWriter
	n int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.n++

	return 0, errors.New("client went away")
}

func TestEventWriterStopsAfterAFailedWrite(t *testing.T) {
	fw := &failingWriter{ResponseWriter: httptest.NewRecorder()}
	ew := NewEventWriter(fw)

	first := ew.Write(Event{Type: EventInit})
	second := ew.Write(Event{Type: EventStdout, Text: "x"})

	if first == nil || second == nil || fw.n != 1 {
		t.Fatalf("errors %v / %v after %d writes, want the first error repeated without another write", first, second, fw.n)
	}
}
