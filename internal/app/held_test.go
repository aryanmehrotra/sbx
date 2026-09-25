package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `sbx sleep` and `sbx wake` go straight to the provider, not through the daemon, so they must
// read the API's own record of the pause: a sleep would docker-stop a sandbox the API froze to
// keep its memory, and a wake would thaw it while the API still reports Paused.
func TestSleepAndWakeRefuseAnAPIPausedSandbox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SBX_HISTORY", filepath.Join(t.TempDir(), "history.jsonl"))

	const id = "osb-0123456789ab"

	dir := filepath.Join(os.Getenv("HOME"), ".sbx", "osb")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	write := func(paused bool) {
		body := `{"id":"` + id + `","state":"Running","pausedByApi":false}`
		if paused {
			body = `{"id":"` + id + `","state":"Paused","pausedByApi":true}`
		}

		if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(true)

	// A socket nothing listens on: if the refusal did not come first, the provider would be
	// reached and the error would be about docker instead.
	for _, verb := range []string{"sleep", "wake", "ready"} {
		err := dispatch(verb, []string{id, "--socket", "unix:///nonexistent/sbx-test.sock"})
		if err == nil || !strings.Contains(err.Error(), "/v1/sandboxes/"+id+"/resume") {
			t.Errorf("sbx %s on an API-paused sandbox = %v, want a refusal pointing at the API resume", verb, err)
		}
	}

	write(false)

	if err := dispatch("sleep", []string{id, "--socket", "unix:///nonexistent/sbx-test.sock"}); err != nil &&
		strings.Contains(err.Error(), "resume") {
		t.Fatalf("an unpaused sandbox was refused as paused: %v", err)
	}
}
