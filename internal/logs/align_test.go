package logs

// The label column has to line up, and it has to stay lined up.
//
// The daemon printed a ragged left edge for its entire life: Align was only ever called by
// `sbx logs`, so in the daemon the width stayed 0 and every message started wherever the
// sandbox and service names happened to end. Reported from a screenshot of a real session,
// where six consecutive lines each began at a different column.

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"
)

// messageColumns returns the column each message starts at, with escape codes removed.
func messageColumns(t *testing.T, raw string) []int {
	t.Helper()

	esc := regexp.MustCompile(`\x1b\[[0-9;]*m`)

	var cols []int

	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		plain := esc.ReplaceAllString(line, "")

		i := strings.Index(plain, "listening")
		if i < 0 {
			t.Fatalf("no message in %q", plain)
		}

		cols = append(cols, i)
	}

	return cols
}

func TestLabelsOfDifferentLengthsShareOneColumn(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)
	l.tty = true // the column only exists in the terminal format; JSON has fields instead

	// Widest first, then narrower: the narrow ones have to be padded out to meet it.
	l.Align(len("selftest-12541") + 1 + len("clickhouse"))

	l.Info("selftest-12541", "clickhouse", "listening on :20000")
	l.Info("zn-dev", "redis", "listening on :20003")
	l.Info("a", "b", "listening on :20020")

	cols := messageColumns(t, buf.String())

	for i, c := range cols {
		if c != cols[0] {
			t.Errorf("line %d starts at column %d, the first at %d - the left edge is ragged, "+
				"which is what the label column exists to prevent", i+1, c, cols[0])
		}
	}
}

func TestTheColumnNeverShrinks(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)
	l.tty = true

	l.Align(30)
	l.Info("a", "b", "listening on :1")

	// A narrower request, as the daemon makes every tick once the longest-named sandbox is
	// removed. Honouring it would re-indent everything after it.
	l.Align(4)
	l.Info("a", "b", "listening on :2")

	cols := messageColumns(t, buf.String())

	if cols[0] != cols[1] {
		t.Errorf("the column moved from %d to %d after a narrower Align, so every line already "+
			"on screen is now misaligned against the new ones", cols[0], cols[1])
	}
}

// An observer runs after the logger's mutex is released.
//
// It is registered with a defer that is deliberately placed before the lock is taken, so LIFO
// ordering runs it last. Get that wrong and an observer which logs - or which writes a file
// that logs - deadlocks the process on its first event, which is the kind of bug that only
// shows up in the daemon at 3am.
func TestAnObserverCanLogWithoutDeadlocking(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)

	done := make(chan struct{})

	l.Observe(func(e Entry) {
		if e.Event == "woke" {
			l.Info("x", "y", "an observer logging from inside an observer")
			close(done)
		}
	})

	go func() {
		l.Event(LevelInfo, "x", "redis", "woke", 191, "woke in 191ms")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the observer never ran, or deadlocked against the logger's own mutex")
	}
}

func TestObserversSeeTheMachineReadableFields(t *testing.T) {
	var buf bytes.Buffer

	l := New(&buf)

	var got Entry

	l.Observe(func(e Entry) { got = e })

	l.Event(LevelInfo, "feature-x", "redis", "woke", 191, "woke in 191ms")

	if got.Event != "woke" || got.DurationMs != 191 {
		t.Errorf("an observer got %+v - the tag and the duration are the whole reason it does "+
			"not have to parse the sentence", got)
	}

	if got.Sandbox != "feature-x" || got.Service != "redis" {
		t.Errorf("an observer cannot tell which sandbox this was: %+v", got)
	}
}
