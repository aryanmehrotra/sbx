//go:build unix

package execd

// Seal and re-key: execd's side of a Firecracker snapshot. The protocol and why it exists are in
// internal/execdctl; this is the enforcement.
//
// The one property everything here serves: a clone restored from a snapshot never answers a
// client with its parent's credentials. Seal is captured by the snapshot, so a clone comes up
// refusing clients; only a re-key - authorised by the control secret, which the re-key also
// rotates - lets clients back in, and only with the new token.
//
// Randomness: execd derives nothing long-lived from it besides the token and the secret, both
// replaced here. Every id it mints (commands, sessions, PTYs) comes from crypto/rand per call,
// which reads the kernel's RNG, and the kernel reseeds per clone through VMGenID - measured
// distinct in every clone in the spike. What cannot be reseeded is the Go runtime's own
// per-process seed (map hashing, select order); nothing in execd uses math/rand, and that seed
// is not a credential.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/aryanmehrotra/sbx/internal/execdctl"
)

// control is the state behind the control endpoints, guarded by mu. mu also serialises claim,
// which rewrites the same env and token.
type control struct {
	mu sync.Mutex

	secret    []byte // the current control secret; empty disables seal and re-key
	vsockOnly bool

	// gen is the last applied re-key's generation. prev and last let that exact re-key be
	// replayed under the secret it was sent with, which is the host's retry when the response
	// was lost; seal clears them, so a replay captured in a snapshot cannot unseal a clone.
	gen  uint64
	prev []byte
	last []byte // sha256 of the last applied request

	// identityEnv is every key claim and re-key have set, so the next re-key can replace them
	// without touching the image's own env.
	identityEnv map[string]bool
}

// authState is one generation of the access token: the token and the context every request it
// authorised runs under. Replacing the token cancels the context.
type authState struct {
	token  []byte
	ctx    context.Context
	cancel context.CancelFunc
}

func newAuth(tok []byte) *authState {
	ctx, cancel := context.WithCancel(context.Background())
	return &authState{token: tok, ctx: ctx, cancel: cancel}
}

// setToken installs tok. A different token ends every request the old one authorised; the same
// token - the parent re-keyed with its own identity after a snapshot - is not a change of auth
// and leaves them running.
func (s *Server) setToken(tok []byte) {
	old := s.auth.Load()
	if subtle.ConstantTimeCompare(old.token, tok) == 1 {
		return
	}

	s.auth.Store(newAuth(tok))
	old.cancel()
}

func writeSealed(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, execdctl.CodeSealed,
		"this sandbox was snapshotted and is waiting for its host to re-key it; retry once the restore completes")
}

// authorizeControl checks the transport and the presence of a control secret, and returns the
// secret the request presented. It writes the refusal itself; ok=false means stop.
func (s *Server) authorizeControl(w http.ResponseWriter, r *http.Request) (presented []byte, ok bool) {
	if len(s.ctl.secret) == 0 {
		writeError(w, http.StatusForbidden, execdctl.CodeControlDisabled,
			"execd was started without "+execdctl.EnvControlSecret+", so there is no host that may seal or re-key it; "+
				"start it with one to enable "+execdctl.PathSeal+" and "+execdctl.PathRekey)

		return nil, false
	}

	if s.ctl.vsockOnly && !overVsock(r.Context()) {
		writeError(w, http.StatusForbidden, execdctl.CodeControlTransport,
			r.URL.Path+" is accepted only on execd's vsock listener, where the VM's host is the only peer; "+
				"send it through the sandbox's vsock device")

		return nil, false
	}

	return []byte(r.Header.Get(execdctl.SecretHeader)), true
}

func matches(presented, secret []byte) bool {
	return len(presented) > 0 && len(secret) > 0 && subtle.ConstantTimeCompare(presented, secret) == 1
}

func writeControlUnauthorized(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, codeUnauthorized,
		"invalid or missing header "+execdctl.SecretHeader+"; use this sandbox's current control secret "+
			"(the one its last re-key installed)")
}

// seal refuses every client call until the next re-key. The host calls it right before
// snapshot/create. It does not end what is being served: a live sandbox being snapshotted keeps
// its running commands, and in a clone the re-key ends them (its token differs).
func (s *Server) seal(w http.ResponseWriter, r *http.Request) {
	s.ctl.mu.Lock()
	defer s.ctl.mu.Unlock()

	presented, ok := s.authorizeControl(w, r)
	if !ok {
		return
	}

	if !matches(presented, s.ctl.secret) {
		writeControlUnauthorized(w)
		return
	}

	s.sealed.Store(true)

	// A replay of the last re-key must not unseal this snapshot's clones.
	s.ctl.prev, s.ctl.last = nil, nil

	w.WriteHeader(http.StatusNoContent)
}

// rekey installs a restore's identity: token, env, control secret; then unseals.
func (s *Server) rekey(w http.ResponseWriter, r *http.Request) {
	s.ctl.mu.Lock()
	defer s.ctl.mu.Unlock()

	presented, ok := s.authorizeControl(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid re-key: read body: "+err.Error())
		return
	}

	var req execdctl.Rekey
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid re-key: "+err.Error())
		return
	}

	// Over the decoded request re-encoded, not the raw bytes: a retry that re-marshals the same
	// Rekey is the same request even if the host's encoder ordered its fields differently.
	canon, _ := json.Marshal(req)
	digest := sha256.Sum256(canon)

	isLast := s.ctl.last != nil && req.Generation == s.ctl.gen && subtle.ConstantTimeCompare(digest[:], s.ctl.last) == 1

	switch {
	case matches(presented, s.ctl.secret):
		if req.Generation <= s.ctl.gen {
			if isLast {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			writeError(w, http.StatusConflict, execdctl.CodeStaleGeneration,
				fmt.Sprintf("re-key generation %d is not newer than %d, the last applied; "+
					"another re-key got here first, or this one is a delayed duplicate - send generation %d or higher",
					req.Generation, s.ctl.gen, s.ctl.gen+1))

			return
		}
	case matches(presented, s.ctl.prev) && isLast:
		// The retry of the re-key that retired this secret: already done, nothing to change.
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		writeControlUnauthorized(w)
		return
	}

	if req.AccessToken == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"invalid re-key: accessToken is required - a restored sandbox must not come up without authentication")

		return
	}

	if len(req.ControlSecret) < execdctl.MinSecretLen {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("invalid re-key: controlSecret must be at least %d characters; use execdctl.NewSecret",
				execdctl.MinSecretLen))

		return
	}

	if subtle.ConstantTimeCompare([]byte(req.ControlSecret), s.ctl.secret) == 1 {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"invalid re-key: controlSecret must differ from the current one - every clone of a snapshot holds that one")

		return
	}

	grace, msg := checkIdentityEnvs(req.Envs)
	if msg != "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid re-key: "+msg)
		return
	}

	if req.Envs != nil {
		if err := s.applyIdentityEnvs(req.Envs, grace, true); err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid re-key: "+err.Error())
			return
		}
	}

	s.ctl.prev, s.ctl.secret = s.ctl.secret, []byte(req.ControlSecret)
	s.ctl.gen, s.ctl.last = req.Generation, digest[:]

	// Token before unseal: the first client let through already meets the new token.
	s.setToken([]byte(req.AccessToken))
	s.sealed.Store(false)

	w.WriteHeader(http.StatusNoContent)
}
