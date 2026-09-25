//go:build unix

package execd

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
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
