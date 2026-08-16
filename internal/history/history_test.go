package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempJournal(t *testing.T) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "history.jsonl")
	t.Setenv("SBX_HISTORY", p)

	return p
}

func TestARecordSurvivesARoundTrip(t *testing.T) {
	tempJournal(t)

	Append(Record{Kind: "command", Sandbox: "feature-x", Command: []string{"sbx", "create", "feature-x"}})
	Append(Record{Kind: "event", Sandbox: "feature-x", Service: "redis", Event: "woke", DurationMs: 191})

	got, err := Read(Filter{})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("wrote 2 records, read %d", len(got))
	}

	if got[1].Event != "woke" || got[1].DurationMs != 191 {
		t.Errorf("the event lost its fields: %+v", got[1])
	}

	if got[0].Time.IsZero() {
		t.Error("a record with no time was written without one, so it cannot be ordered")
	}
}

// The whole point of an audit log is that it is there afterwards, so it must not be possible
// for a secret to get into it.
func TestSecretsAreNotWrittenDown(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		gone string
	}{
		{
			name: "--env with a separate value",
			argv: []string{"sbx", "add", "x", "db", "--env", "POSTGRES_PASSWORD=hunter2"},
			gone: "hunter2",
		},
		{
			name: "--env=K=V",
			argv: []string{"sbx", "add", "x", "db", "--env=API_TOKEN=abc123"},
			gone: "abc123",
		},
		{
			name: "several pairs in one value",
			argv: []string{"sbx", "add", "x", "db", "--env", "PORT=5432,DB_SECRET=swordfish"},
			gone: "swordfish",
		},
		{
			name: "a bare assignment anywhere",
			argv: []string{"sbx", "create", "x", "AWS_SECRET_ACCESS_KEY=zzz"},
			gone: "zzz",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(Redact(c.argv), " ")

			if strings.Contains(got, c.gone) {
				t.Errorf("the secret survived redaction: %q", got)
			}

			if !strings.Contains(got, "***") {
				t.Errorf("nothing was redacted: %q", got)
			}
		})
	}
}

// Redaction that eats everything is its own failure: the log has to stay useful.
func TestOrdinaryArgumentsAreKept(t *testing.T) {
	argv := []string{"sbx", "add", "x", "cache", "--image", "redis:7-alpine", "--port", "6379",
		"--env", "REDIS_DB=0"}

	got := strings.Join(Redact(argv), " ")

	for _, want := range []string{"add", "cache", "redis:7-alpine", "6379", "REDIS_DB=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction removed %q, which is not a secret: %q", want, got)
		}
	}

	// The name of a secret is the useful half and is deliberately kept.
	kept := strings.Join(Redact([]string{"--env", "DB_PASSWORD=x"}), " ")
	if !strings.Contains(kept, "DB_PASSWORD") {
		t.Errorf("the variable's name was removed too, so the record says nothing: %q", kept)
	}
}

func TestFiltering(t *testing.T) {
	tempJournal(t)

	Append(Record{Kind: "command", Sandbox: "a", Command: []string{"sbx", "create", "a"}})
	Append(Record{Kind: "event", Sandbox: "b", Event: "woke"})
	Append(Record{Kind: "event", Sandbox: "a", Event: "slept"})

	bySandbox, err := Read(Filter{Sandbox: "a"})
	if err != nil {
		t.Fatal(err)
	}

	if len(bySandbox) != 2 {
		t.Errorf("filtering by sandbox returned %d records, want 2", len(bySandbox))
	}

	byKind, err := Read(Filter{Kind: "event"})
	if err != nil {
		t.Fatal(err)
	}

	if len(byKind) != 2 {
		t.Errorf("filtering by kind returned %d records, want 2", len(byKind))
	}
}

// The most recent records are the ones a limit must keep. Dropping the newest and showing
// somebody last week would be worse than showing nothing.
func TestALimitKeepsTheNewest(t *testing.T) {
	tempJournal(t)

	for i := range 10 {
		Append(Record{Kind: "event", Event: "woke", DurationMs: int64(i)})
	}

	got, err := Read(Filter{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("limit 3 returned %d", len(got))
	}

	if got[2].DurationMs != 9 {
		t.Errorf("the last record is %d, want the newest (9) - the limit kept the wrong end",
			got[2].DurationMs)
	}
}

// A line the writer never finished, as happens when a machine is powered off mid-append, must
// not hide every record after it.
func TestATruncatedLineDoesNotEndTheRead(t *testing.T) {
	p := tempJournal(t)

	Append(Record{Kind: "event", Event: "woke"})

	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = f.WriteString(`{"kind":"event","ev`)
	_, _ = f.WriteString("\n")
	_ = f.Close()

	Append(Record{Kind: "event", Event: "slept"})

	got, err := Read(Filter{})
	if err != nil {
		t.Fatalf("a half-written line failed the whole read: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("read %d records around a truncated line, want 2", len(got))
	}
}

// A missing journal is the state of every machine before the first command, and is not an
// error to report at people.
func TestNoJournalYetIsNotAnError(t *testing.T) {
	t.Setenv("SBX_HISTORY", filepath.Join(t.TempDir(), "nothing-here.jsonl"))

	got, err := Read(Filter{})
	if err != nil {
		t.Fatalf("reading a journal that does not exist returned %v", err)
	}

	if len(got) != 0 {
		t.Errorf("got %d records from a file that is not there", len(got))
	}
}

func TestTheFileIsNotWorldReadable(t *testing.T) {
	p := tempJournal(t)

	Append(Record{Kind: "command", Command: []string{"sbx", "list"}})

	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the journal is mode %o - it records what was run and where, which is nobody "+
			"else's business on a shared machine", mode)
	}
}

// Guards the on-disk shape, which `sbx history --json | jq` and anything else reading the
// file depend on.
func TestTheOnDiskShapeIsStable(t *testing.T) {
	p := tempJournal(t)

	Append(Record{
		Time: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		Kind: "event", Sandbox: "x", Service: "redis", Event: "woke", DurationMs: 191,
	})

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"time", "kind", "sandbox", "service", "event", "durationMs"} {
		if _, ok := m[k]; !ok {
			t.Errorf("the record has no %q field", k)
		}
	}

	// Empty fields are omitted, so a journal of a million events does not carry a million
	// empty strings.
	if _, ok := m["command"]; ok {
		t.Error("an unset field was written anyway")
	}
}
