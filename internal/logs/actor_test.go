package logs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// An audit log that cannot say who did something is not an audit log.
//
// The daemon sleeping a service on idle and a person sleeping it from `sbx ui` produce the
// same word. Told apart only by that word, an hour goes into looking for a bug in the reaper
// that was really somebody pressing a key — which is the case this field exists for.
func TestActorAppearsInJSON(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)
	l.ActorEvent(LevelInfo, ActorUI, "br", "redis", "slept", 0, "asleep")

	var got struct {
		Actor   string `json:"actor"`
		Event   string `json:"event"`
		Service string `json:"service"`
	}

	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}

	if got.Actor != ActorUI {
		t.Errorf("actor=%q, want %q", got.Actor, ActorUI)
	}

	if got.Event != "slept" {
		t.Errorf("event=%q, want slept", got.Event)
	}
}

// Event keeps meaning what it meant. Every existing caller is the daemon, so an omitted actor
// reads as the daemon rather than as a blank that a reader has to interpret.
func TestEventDefaultsToDaemonActor(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)
	l.Event(LevelInfo, "br", "redis", "woke", 12, "woke in 12ms")

	var got struct {
		Actor string `json:"actor"`
	}

	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}

	if got.Actor != ActorDaemon {
		t.Errorf("actor=%q, want %q", got.Actor, ActorDaemon)
	}
}

// The observer is how history is written, and it fires below the printing threshold on
// purpose: the dashboard owns the screen, so it raises the level to stay silent — and an
// audit record that disappeared because the UI went quiet would be the opposite of the point.
func TestObserverSeesActorEvenWhenNotPrinted(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)
	l.SetLevel(LevelError) // printing threshold above the event below

	var seen []Entry

	l.Observe(func(e Entry) { seen = append(seen, e) })
	l.ActorEvent(LevelInfo, ActorUI, "br", "redis", "removed", 0, "removed")

	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("printed %q, want nothing", got)
	}

	if len(seen) != 1 {
		t.Fatalf("observer saw %d entries, want 1", len(seen))
	}

	if seen[0].Actor != ActorUI || seen[0].Event != "removed" {
		t.Errorf("entry actor=%q event=%q, want %s/removed", seen[0].Actor, seen[0].Event, ActorUI)
	}
}

// SetLevel returns what it replaced so a caller can put back what was actually there.
//
// The dashboard silences the terminal it draws on and restores the level on the way out.
// Restoring a hardcoded LevelInfo instead would quietly undo a --debug the user asked for,
// and the symptom - logs stop being verbose after opening the dashboard once - looks nothing
// like its cause.
func TestSetLevelReturnsPrevious(t *testing.T) {
	l := New(&bytes.Buffer{})

	l.SetLevel(LevelDebug)

	was := l.SetLevel(LevelSilent)
	if was != LevelDebug {
		t.Fatalf("SetLevel returned %v, want %v", was, LevelDebug)
	}

	// Putting it back must restore what was there, which is the whole point of the return.
	if again := l.SetLevel(was); again != LevelSilent {
		t.Errorf("SetLevel returned %v, want %v", again, LevelSilent)
	}

	var buf bytes.Buffer

	l2 := New(&buf)
	l2.SetLevel(LevelDebug)
	l2.SetLevel(l2.SetLevel(LevelSilent)) // silence, then restore what it replaced
	l2.Event(LevelDebug, "br", "redis", "woke", 1, "debug line")

	if buf.Len() == 0 {
		t.Error("debug line was dropped after restore; the level was not put back")
	}
}

// LevelSilent must be above every real level, or the dashboard still gets written on.
func TestLevelSilentIsAboveFatal(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)
	l.SetLevel(LevelSilent)
	l.Event(LevelFatal, "br", "redis", "died", 0, "fatal line")

	if got := buf.String(); got != "" {
		t.Errorf("printed %q at LevelSilent, want nothing", got)
	}
}
