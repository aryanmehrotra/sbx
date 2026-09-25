package app

import (
	"embed"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPCheckExitsOnStatus(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer ok.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()

	if got := httpcheck([]string{ok.URL}); got != 0 {
		t.Errorf("200 -> exit %d, want 0", got)
	}

	if got := httpcheck([]string{bad.URL}); got != 1 {
		t.Errorf("503 -> exit %d, want 1", got)
	}

	if got := httpcheck([]string{"http://127.0.0.1:1/"}); got != 1 {
		t.Errorf("refused -> exit %d, want 1", got)
	}
}

// A health check runs every few seconds for a sandbox's whole life. Recorded like a host
// command, it would grow a journal inside the container without bound.
func TestInContainerCommandsLeaveNoJournal(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "history.jsonl")
	t.Setenv("SBX_HISTORY", journal)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	// Main sets package state the template tests read; put it back.
	oldT, oldV := templates, version
	defer func() { templates, version = oldT, oldV }()

	if code := Main("test", embed.FS{}, []string{"sbx", "httpcheck", srv.URL}); code != 0 {
		t.Fatalf("exit %d", code)
	}

	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("httpcheck wrote the journal (%v); in-container commands must not", err)
	}
}
