//go:build unix

package execd

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// A warm-pool sandbox is started with a token only sbx serve knows. Claiming it hands it to one
// caller: a new token, which alone works from then on, and the caller's env, which every command
// sees exactly as if it had been passed to `docker run -e`.
func TestClaimSwapsTokenAndAppliesEnv(t *testing.T) {
	const pool, mine = "pool-token", "caller-token"

	// Set here so the test restores it; the claim is what actually sets it.
	t.Setenv("SBX_CLAIM_TEST_VAR", "")
	os.Unsetenv("SBX_CLAIM_TEST_VAR")

	s := newTestServer(t, Options{AccessToken: pool})

	claim := map[string]any{"accessToken": mine, "envs": map[string]string{"SBX_CLAIM_TEST_VAR": "from-claim"}}

	if st, _, body := s.do("POST", "/sbx/claim", claim, AccessTokenHeader, "wrong"); st != http.StatusUnauthorized {
		t.Fatalf("claim with a wrong token: %d %s, want 401", st, body)
	}

	if st, _, body := s.do("POST", "/sbx/claim", claim, AccessTokenHeader, pool); st != http.StatusNoContent {
		t.Fatalf("claim: %d %s, want 204", st, body)
	}

	cmd := map[string]any{"command": "echo $SBX_CLAIM_TEST_VAR"}

	if st, _, _ := s.do("POST", "/command", cmd, AccessTokenHeader, pool); st != http.StatusUnauthorized {
		t.Fatalf("the pool token still works after the claim: %d", st)
	}

	st, _, data := s.do("POST", "/command", cmd, AccessTokenHeader, mine)
	if st != http.StatusOK {
		t.Fatalf("command with the claimed token: %d %s", st, data)
	}

	ex, err := parseExecution(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(ex.Text()); got != "from-claim" {
		t.Fatalf("command saw %q, want the claimed env value", got)
	}

	// Once: whoever holds the sandbox now must not be able to re-key it out from under the
	// endpoint sbx serve hands out.
	again := map[string]any{"accessToken": "third"}
	st, _, body := s.do("POST", "/sbx/claim", again, AccessTokenHeader, mine)
	wantError(t, st, body, http.StatusConflict, "ALREADY_CLAIMED")
}

func TestClaimRefusesAnUnkeyedServerAndABadBody(t *testing.T) {
	open := newTestServer(t, Options{})

	st, _, body := open.do("POST", "/sbx/claim", map[string]any{"accessToken": "x"})
	wantError(t, st, body, http.StatusConflict, "ALREADY_CLAIMED")

	s := newTestServer(t, Options{AccessToken: "p"})

	st, _, body = s.do("POST", "/sbx/claim", map[string]any{"accessToken": ""}, AccessTokenHeader, "p")
	wantError(t, st, body, http.StatusBadRequest, codeInvalidRequest)

	st, _, body = s.do("POST", "/sbx/claim", map[string]any{"accessToken": "n", "envs": map[string]string{"A=B": "c"}},
		AccessTokenHeader, "p")
	wantError(t, st, body, http.StatusBadRequest, codeInvalidRequest)
}

// upstream's e2e suite sets EXECD_API_GRACE_SHUTDOWN on every sandbox it creates. From a claim it
// becomes execd's grace - read at shutdown, not at start - and, as with `docker run -e`, stays
// out of the environment commands see.
func TestClaimAppliesTheShutdownGraceWithoutExportingIt(t *testing.T) {
	old := shutdownGrace.Load()
	t.Cleanup(func() { shutdownGrace.Store(old) })

	s := newTestServer(t, Options{AccessToken: "p"})

	bad := map[string]any{"accessToken": "n", "envs": map[string]string{EnvGraceShutdown: "soon"}}
	st, _, body := s.do("POST", "/sbx/claim", bad, AccessTokenHeader, "p")
	wantError(t, st, body, http.StatusBadRequest, codeInvalidRequest)

	ok := map[string]any{"accessToken": "n", "envs": map[string]string{EnvGraceShutdown: "3s"}}
	if st, _, body := s.do("POST", "/sbx/claim", ok, AccessTokenHeader, "p"); st != http.StatusNoContent {
		t.Fatalf("claim: %d %s", st, body)
	}

	if got := time.Duration(shutdownGrace.Load()); got != 3*time.Second {
		t.Fatalf("grace %s after the claim, want 3s", got)
	}

	st, _, data := s.do("POST", "/command", map[string]any{"command": "echo ${EXECD_API_GRACE_SHUTDOWN:-unset}"},
		AccessTokenHeader, "n")
	if st != http.StatusOK {
		t.Fatalf("command: %d %s", st, data)
	}

	ex, err := parseExecution(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(ex.Text()); got != "unset" {
		t.Fatalf("commands see EXECD_API_GRACE_SHUTDOWN=%q; it is execd's setting, not theirs", got)
	}
}
