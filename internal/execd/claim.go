package execd

// The warm-pool claim: an sbx extension, not part of OpenSandbox's execd API.
//
// sbx serve keeps sandboxes created ahead of demand, execd already answering, so a create can
// hand one out in milliseconds instead of starting a container. What such a sandbox cannot have
// yet is anything that belongs to the caller: the access token (minted per sandbox, and the only
// credential for everything inside it) and the env the caller asked for. This endpoint installs
// both, once, authenticated by the token the pool member was started with - which only sbx serve
// ever held.

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

type claimRequest struct {
	AccessToken string            `json:"accessToken"`
	Envs        map[string]string `json:"envs"`
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid claim: "+err.Error())
		return
	}

	if req.AccessToken == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"invalid claim: accessToken is required - it becomes this sandbox's only token")

		return
	}

	for k := range req.Envs {
		if k == "" || strings.ContainsAny(k, "=\x00") || hiddenEnv[k] {
			writeError(w, http.StatusBadRequest, codeInvalidRequest,
				"invalid claim: env key "+k+" is not a variable a sandbox may set")

			return
		}
	}

	// Once. After this the caller holds the sandbox, and a second claim would let them re-key
	// it out from under the endpoint sbx serve hands out.
	if !s.claimed.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "ALREADY_CLAIMED",
			"this sandbox has already been claimed; its token and env are set for good")

		return
	}

	// Into the process environment, not a side table: userEnv starts from os.Environ on every
	// command, session and path expansion, so this is exactly what `docker run -e` would have
	// given them - one mechanism, and nothing that could forget to consult a second.
	for k, v := range req.Envs {
		if err := os.Setenv(k, v); err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid claim: env "+k+": "+err.Error())
			return
		}
	}

	tok := []byte(req.AccessToken)
	s.token.Store(&tok)

	w.WriteHeader(http.StatusNoContent)
}
