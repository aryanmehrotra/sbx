// Package execdctl is the host's side of execd's control endpoints: seal before a snapshot,
// re-key after a restore. It holds the wire shapes so execd (the guest) and the provider (the
// host) cannot drift apart, and a small client that speaks them over any dialer - in practice
// internal/fcvsock, since these calls travel over the VM's vsock device.
//
// Why re-key at all: a Firecracker snapshot captures userspace exactly, so every clone restored
// from one snapshot starts with the same execd access token, the same control secret and the
// same env. The kernel reseeds its RNG per clone (VMGenID), userspace does not - measured in
// docs/superpowers/specs/2026-09-26-firecracker-spike.md. Without a re-key, a caller holding one
// clone's token holds all of them.
//
// The protocol, host's view:
//
//	before snapshot/create:  Seal(secret)                       - execd refuses every client call
//	after  snapshot/load:    Rekey(secret, {gen+1, token, next}) - new token, new secret, unsealed
//
// Seal is what makes the order safe rather than merely intended: a sealed execd is captured in
// the snapshot, so every clone wakes refusing clients (503, and /ping too, so a health probe
// reads "not serving") until its own Rekey lands. A host that forgets to re-key gets a clone that
// answers nobody - never one that answers with its parent's token.
package execdctl

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	// EnvControlSecret is the boot-time control secret execd reads (and removes from its own
	// environment) at start. Unset means seal and re-key are refused: nothing can re-key it.
	EnvControlSecret = "EXECD_CONTROL_SECRET"

	// SecretHeader carries the current control secret on Seal and Rekey.
	SecretHeader = "X-Sbx-Control-Secret"

	// PathSeal and PathRekey are the endpoints, beside the warm pool's /sbx/claim.
	PathSeal  = "/sbx/seal"
	PathRekey = "/sbx/rekey"

	// MinSecretLen is the shortest control secret execd accepts: 128 bits as hex. NewSecret
	// returns 64 characters.
	MinSecretLen = 32
)

// Error codes execd answers the control endpoints with, beside upstream's UNAUTHORIZED and
// INVALID_REQUEST_BODY.
const (
	CodeSealed           = "SEALED"
	CodeStaleGeneration  = "STALE_GENERATION"
	CodeControlDisabled  = "CONTROL_DISABLED"
	CodeControlTransport = "CONTROL_TRANSPORT"
)

// Rekey is one restore's new identity.
//
// Generation orders re-keys: execd applies one only if it is greater than the last applied, so a
// delayed or duplicated request can never roll a sandbox back to an older identity. Resending
// the very same Rekey with the secret it was sent with is a no-op success - the idempotent retry
// for a response lost on the wire.
type Rekey struct {
	Generation uint64 `json:"generation"`

	// AccessToken replaces execd's access token. Required: a restored sandbox must not come up
	// with no authentication.
	AccessToken string `json:"accessToken"`

	// ControlSecret replaces the secret that authorised this call, so a secret captured in the
	// snapshot is dead in every clone the moment it is re-keyed. Required, and must differ.
	ControlSecret string `json:"controlSecret"`

	// Envs, when non-nil, replaces the env execd's identity calls (claim and re-key) set before:
	// keys they set that are absent here are removed, keys here are set. nil leaves the env as
	// it is. The image's own env is never touched either way.
	Envs map[string]string `json:"envs"`
}

// NewSecret returns 256 random bits as hex, for a token or a control secret.
func NewSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes for a secret: %w", err)
	}

	return hex.EncodeToString(b[:]), nil
}

// Error is a refusal from execd, with its code so a caller can tell a stale generation (someone
// else already re-keyed it) from a wrong secret.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("execd %d %s: %s", e.Status, e.Code, e.Message)
}

// Client calls execd's control endpoints over Dial. Each call opens one connection and closes it:
// these happen once per snapshot and once per restore, and a pooled connection would be one
// that the snapshot itself captured half-open.
type Client struct {
	Dial func(ctx context.Context) (net.Conn, error)

	// Timeout bounds each call when ctx has no sooner deadline. Zero means 10s.
	Timeout time.Duration
}

// Seal makes execd refuse every client call until the next Rekey, and ends the streams it is
// serving. Call it before snapshot/create. Sealing twice is harmless.
func (c Client) Seal(ctx context.Context, secret string) error {
	return c.call(ctx, PathSeal, secret, nil)
}

// Rekey installs r and unseals. Call it after snapshot/load, before the sandbox is handed to
// anyone. On a transport error, retry with the same secret and the same r.
func (c Client) Rekey(ctx context.Context, secret string, r Rekey) error {
	return c.call(ctx, PathRekey, secret, r)
}

func (c Client) call(ctx context.Context, path, secret string, body any) error {
	if c.Dial == nil {
		return fmt.Errorf("execdctl: no Dial; pass the sandbox's vsock dialer (fcvsock.Dialer.DialContext)")
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rd io.Reader = http.NoBody

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}

		rd = bytes.NewReader(b)
	}

	// The host part is never resolved: Dial decides where the request goes.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://execd"+path, rd)
	if err != nil {
		return err
	}

	req.Header.Set(SecretHeader, secret)
	req.Header.Set("Content-Type", "application/json")

	hc := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return c.Dial(ctx) },
		DisableKeepAlives: true,
	}}

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("execdctl: POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}

	e := &Error{Status: resp.StatusCode}

	var eb struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}

	if json.Unmarshal(data, &eb) == nil && eb.Code != "" {
		e.Code, e.Message = eb.Code, eb.Message
	} else {
		e.Message = string(bytes.TrimSpace(data))
	}

	return e
}
