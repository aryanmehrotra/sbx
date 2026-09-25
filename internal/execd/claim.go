//go:build unix

package execd

// The warm-pool claim: an sbx extension, not part of OpenSandbox's execd API.
//
// sbx serve keeps sandboxes created ahead of demand, execd already answering, so a create can
// hand one out in milliseconds instead of starting a container. What such a sandbox cannot have
// yet is anything that belongs to the caller: the access token (minted per sandbox, and the only
// credential for everything inside it) and the env the caller asked for. This endpoint installs
// both, once, authenticated by the token the pool member was started with - which only sbx serve
// ever held.
//
// A restore's re-key (rekey.go) installs the same two things through the same helpers below, so
// the rules for what env a sandbox may be given live in one place.

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
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

	grace, msg := checkIdentityEnvs(req.Envs)
	if msg != "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid claim: "+msg)
		return
	}

	// Serialised with seal and re-key: all three rewrite the env and the token, and interleaved
	// they could leave one caller's token with another's env.
	s.ctl.mu.Lock()
	defer s.ctl.mu.Unlock()

	// Once. After this the caller holds the sandbox, and a second claim would let them re-key
	// it out from under the endpoint sbx serve hands out.
	if !s.claimed.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "ALREADY_CLAIMED",
			"this sandbox has already been claimed; its token and env are set for good")

		return
	}

	if err := s.applyIdentityEnvs(req.Envs, grace, false); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid claim: "+err.Error())
		return
	}

	s.setToken([]byte(req.AccessToken))

	w.WriteHeader(http.StatusNoContent)
}

// checkIdentityEnvs validates env an identity call (claim, re-key) wants to set, before anything
// is changed. It returns the shutdown grace if one was asked for, or why the env is refused.
func checkIdentityEnvs(envs map[string]string) (*time.Duration, string) {
	// The shutdown grace is execd's own setting, read when it started; a caller who set it at
	// create - upstream's e2e suite does, on every sandbox - gets it applied here instead. It
	// stays out of the environment commands see, as it would have from `docker run -e`.
	var grace *time.Duration

	if v, ok := envs[EnvGraceShutdown]; ok {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, EnvGraceShutdown + " must be a Go duration such as 200ms or 2s"
		}

		grace = &d
	}

	for k := range envs {
		if k == EnvGraceShutdown {
			continue
		}

		if k == "" || strings.ContainsAny(k, "=\x00") || hiddenEnv[k] {
			return nil, "env key " + k + " is not a variable a sandbox may set"
		}
	}

	return grace, ""
}

// applyIdentityEnvs sets envs in execd's own environment and remembers the keys as identity env.
// With replace, identity keys set by earlier calls and absent from envs are removed first - the
// image's own env is never in that set, so it is never touched. Call with s.ctl.mu held.
func (s *Server) applyIdentityEnvs(envs map[string]string, grace *time.Duration, replace bool) error {
	// Into the process environment, not a side table: userEnv starts from os.Environ on every
	// command, session and path expansion, so this is exactly what `docker run -e` would have
	// given them - one mechanism, and nothing that could forget to consult a second.
	if grace != nil {
		shutdownGrace.Store(int64(*grace))
	}

	if replace {
		for k := range s.ctl.identityEnv {
			if _, keep := envs[k]; !keep {
				_ = os.Unsetenv(k)
				delete(s.ctl.identityEnv, k)
			}
		}
	}

	for k, v := range envs {
		if k == EnvGraceShutdown {
			continue
		}

		if err := os.Setenv(k, v); err != nil {
			return err
		}

		s.ctl.identityEnv[k] = true
	}

	return nil
}

// shutdownGrace is how long execd keeps serving after its entrypoint exits, in nanoseconds: set
// from EXECD_API_GRACE_SHUTDOWN at start, and by a claim.
var shutdownGrace atomic.Int64
