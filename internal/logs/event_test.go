package logs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A machine reading the stream needs a field it can switch on. Matching "woke in 278ms" out
// of a sentence written for a human is a contract nobody agreed to, and it breaks the first
// time somebody rewords the line.
func TestEventFieldsAppearInJSON(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)
	l.Event(LevelInfo, "br", "redis", "woke", 278, "woke in %dms", 278)

	var got struct {
		Sandbox    string `json:"sandbox"`
		Service    string `json:"service"`
		Event      string `json:"event"`
		DurationMs int64  `json:"durationMs"`
		Message    string `json:"message"`
	}

	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}

	if got.Event != "woke" || got.DurationMs != 278 {
		t.Errorf("event=%q durationMs=%d, want woke/278", got.Event, got.DurationMs)
	}

	// The human sentence must survive alongside the machine fields, not be replaced by them.
	if got.Message != "woke in 278ms" {
		t.Errorf("message=%q, want %q", got.Message, "woke in 278ms")
	}

	if got.Sandbox != "br" || got.Service != "redis" {
		t.Errorf("sandbox=%q service=%q, want br/redis", got.Sandbox, got.Service)
	}
}

// Every line that existed before this change must serialise exactly as it did. `omitempty`
// is what makes that true, and this is the test that would fail if someone removed it.
func TestPlainLogHasNoEventFields(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)
	l.Info("br", "redis", "listening on :20000")

	out := buf.String()
	if strings.Contains(out, "event") || strings.Contains(out, "durationMs") {
		t.Errorf("a non-event line carried event fields:\n%s", out)
	}
}

// An event with no duration — a failed wake — must not report a zero-millisecond one. A
// histogram observation of 0 ms for a wake that never happened is worse than no observation.
func TestZeroDurationIsOmitted(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)
	l.Event(LevelError, "br", "redis", "wakeFailed", 0, "could not wake: %v", "boom")

	out := buf.String()
	if strings.Contains(out, "durationMs") {
		t.Errorf("a zero duration was serialised:\n%s", out)
	}

	if !strings.Contains(out, `"event":"wakeFailed"`) {
		t.Errorf("the event tag is missing:\n%s", out)
	}
}

// The last line of a container that was killed mid-write is very often the one somebody is
// running `sbx logs` to read. It has no trailing newline, so it sits in the buffer waiting
// for one that never comes.
func TestFlushEmitsATrailingPartialLine(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)
	w := &LineWriter{Log: l, Sandbox: "s", Service: "svc", Level: LevelInfo}

	if _, err := w.Write([]byte("first line\nPANIC: the useful bit")); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(buf.String(), "PANIC") {
		t.Fatal("an unterminated line was emitted before Flush, so this test proves nothing")
	}

	w.Flush()

	if !strings.Contains(buf.String(), "PANIC: the useful bit") {
		t.Errorf("the final unterminated line was dropped:\n%s", buf.String())
	}

	// Flushing twice must not repeat it.
	before := buf.String()
	w.Flush()

	if buf.String() != before {
		t.Error("a second Flush emitted the line again")
	}
}
