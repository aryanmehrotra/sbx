package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
)

func journal(t *testing.T) {
	t.Helper()
	t.Setenv("SBX_HISTORY", filepath.Join(t.TempDir(), "history.jsonl"))
}

// durationMs means different things per event, and one preposition for both states something
// false: a sleep's number is how long the sandbox sat idle first, not how long sleeping took.
func TestASleepIsNotDescribedAsHavingTakenTime(t *testing.T) {
	journal(t)

	history.Append(history.Record{Kind: "event", Sandbox: "x", Service: "redis",
		Event: "slept", DurationMs: 8000})
	history.Append(history.Record{Kind: "event", Sandbox: "x", Service: "redis",
		Event: "woke", DurationMs: 191})

	var buf bytes.Buffer
	if err := History(nil, &buf); err != nil {
		t.Fatal(err)
	}

	got := buf.String()

	if strings.Contains(got, "slept in") {
		t.Errorf("a sleep is rendered as having taken 8s, which is the idle time before it:\n%s", got)
	}

	if !strings.Contains(got, "slept after 8s idle") {
		t.Errorf("the idle time is not shown:\n%s", got)
	}

	if !strings.Contains(got, "woke in 191ms") {
		t.Errorf("a wake's duration is how long the caller waited and should say so:\n%s", got)
	}
}

func TestAnEmptyJournalSaysSoWithoutFailing(t *testing.T) {
	journal(t)

	var buf bytes.Buffer
	if err := History(nil, &buf); err != nil {
		t.Fatalf("an empty history is the state of every new machine, not an error: %v", err)
	}

	if !strings.Contains(buf.String(), "nothing recorded") {
		t.Errorf("it printed %q instead of saying there is nothing yet", buf.String())
	}
}

func TestFilteringBySandboxName(t *testing.T) {
	journal(t)

	history.Append(history.Record{Kind: "command", Sandbox: "keep", Command: []string{"sbx", "create", "keep"}})
	history.Append(history.Record{Kind: "command", Sandbox: "drop", Command: []string{"sbx", "create", "drop"}})

	var buf bytes.Buffer
	if err := History([]string{"keep"}, &buf); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(buf.String(), "drop") {
		t.Errorf("filtering by sandbox showed another sandbox's records:\n%s", buf.String())
	}

	if !strings.Contains(buf.String(), "keep") {
		t.Errorf("filtering by sandbox hid its own records:\n%s", buf.String())
	}
}

func TestAskingForBothHalvesIsRefused(t *testing.T) {
	journal(t)

	var buf bytes.Buffer

	err := History([]string{"--commands", "--events"}, &buf)
	if err == nil {
		t.Fatal("--commands --events asks for opposite halves and was accepted silently")
	}

	if !strings.Contains(err.Error(), "opposite halves") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// A failed command is the one people go looking for, so it has to be visibly different.
func TestAFailureIsMarked(t *testing.T) {
	journal(t)

	history.Append(history.Record{
		Kind: "command", Sandbox: "x", Command: []string{"sbx", "create", "x"},
		Failed: true, Error: "port 20000 is already in use\nsecond line that should not appear",
	})

	var buf bytes.Buffer
	if err := History(nil, &buf); err != nil {
		t.Fatal(err)
	}

	got := buf.String()

	if !strings.Contains(got, "failed") {
		t.Errorf("a failed command is not marked as one:\n%s", got)
	}

	if !strings.Contains(got, "already in use") {
		t.Errorf("the reason it failed is missing:\n%s", got)
	}

	if strings.Contains(got, "second line") {
		t.Errorf("a multi-line error broke the one-record-per-line layout:\n%s", got)
	}
}

// Records from different days get a header rather than a date on all fifty lines.
func TestDaysAreHeadings(t *testing.T) {
	journal(t)

	history.Append(history.Record{
		Time: time.Date(2026, 8, 15, 9, 0, 0, 0, time.Local),
		Kind: "command", Sandbox: "x", Command: []string{"sbx", "create", "x"},
	})
	history.Append(history.Record{
		Time: time.Date(2026, 8, 16, 9, 0, 0, 0, time.Local),
		Kind: "command", Sandbox: "x", Command: []string{"sbx", "rm", "x"},
	})

	var buf bytes.Buffer
	if err := History(nil, &buf); err != nil {
		t.Fatal(err)
	}

	got := buf.String()

	for _, want := range []string{"Sat 15 Aug 2026", "Sun 16 Aug 2026"} {
		if !strings.Contains(got, want) {
			t.Errorf("no heading for %s:\n%s", want, got)
		}
	}
}
