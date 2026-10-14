// Package credvault holds a sandbox's credentials outside the sandbox.
//
// An agent that calls an API needs a key, and a key in its environment is a key it can print,
// commit or send somewhere else. The vault keeps the key here, in the daemon, and the sandbox
// sends an unauthenticated request to a host its egress policy already permits. Which hosts get
// which credential is a binding, and a binding is the whole configuration surface: hosts,
// schemes, methods, paths, and what to add.
//
// It is OpenSandbox's Credential Vault API (specs/egress-api.yaml at release-1.1.0, the commit
// pinned in test/osb/UPSTREAM), served by sbx's own egress filter rather than by a sidecar inside
// the sandbox.
//
// # Why this is not in internal/egress
//
// Every file of internal/egress is embedded verbatim into the container filter's build context
// (egress/context.go), so a type added there is compiled into a program that runs in a container
// on the sandbox's own bridge, configured over an HTTP endpoint with no redaction. This package
// holds a certificate authority's private key and the plaintext of every credential, and neither
// belongs in that process. Keeping them here means the container filter's sources do not change,
// its binary gains nothing, and the key stays in the process that minted it. The filter reaches
// this package through one interface field instead (egress.CredentialSource).
//
// So a filter running as a container cannot have a vault at all, and that is enforced by where
// vaults are made: the daemon opens one beside the filter it hosts itself (daemon/egressproxy.go)
// and closes it with that filter, so there is no vault to populate on a host whose bridge gateway
// the daemon could not bind. A sandbox asking for one there is refused by name.
//
// Nothing here is persisted. The key and the credentials live in memory and die with the sandbox,
// which is the point: the API's record file is 0600 on disk and survives a restart, and a private
// key that survives a restart is a private key somebody can find.
package credvault

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Secret is credential plaintext. Its type is the redaction: String, GoString and MarshalJSON all
// answer with a placeholder, so a log line, a %v, a %#v dump or a marshalled struct cannot print
// one, and only this package can read the value back (reveal).
//
// It is done this way rather than with a hook in internal/logs because a hook covers the logger
// and nothing else. A secret reaches a docker label, an argv or an error string through fmt, not
// through the logger, and those are the paths that matter: the value is a credential for somebody
// else's API, and once it is in a log file it is theirs too.
type Secret string

const redacted = "[redacted]"

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// MarshalJSON keeps the value out of every response: the API is write-only for credential
// plaintext, and the sanitized state types carry no Secret field at all. This is the second
// layer, for a struct somebody marshals by hand.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

func (s *Secret) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}

	*s = Secret(v)

	return nil
}

// reveal is the plaintext, for this package only. Unexported so that the API handler, the logger
// and the daemon cannot reach it: the only code that needs the value is the code that puts it on
// an outbound request.
func (s Secret) reveal() string { return string(s) }

// The wire types are OpenSandbox's, field for field, with the json names the SDK sends
// (sdks/sandbox/go/types.go at the pinned commit). No field is added: a vault that accepted an
// extra one would be a vault whose configuration means something different from the spec every
// client was written against.

// Credential is one named secret. Source is write-only.
type Credential struct {
	Name   string       `json:"name"`
	Source InlineSource `json:"source"`
}

// InlineSource carries credential material in the request. "inline" is the only source type the
// spec defines at release-1.1.0.
type InlineSource struct {
	Type  string `json:"type"`
	Value Secret `json:"value"`
}

// Match selects the outbound requests a binding applies to.
type Match struct {
	Schemes []string `json:"schemes,omitempty"`

	// Ports is deprecated upstream and ignored: the port is the scheme's (https 443, http 80).
	// A value other than 80 or 443 is refused rather than dropped, because a caller who wrote
	// one meant a port the vault will never use.
	Ports   []int    `json:"ports,omitempty"`
	Hosts   []string `json:"hosts"`
	Methods []string `json:"methods,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

// HeaderEntry is one header of a customHeaders binding.
type HeaderEntry struct {
	Name       string `json:"name"`
	Credential string `json:"credential"`
}

// Substitution replaces a literal placeholder in a named request surface with a credential.
type Substitution struct {
	Credential  string   `json:"credential"`
	Placeholder string   `json:"placeholder"`
	In          []string `json:"in"`
}

// Auth is what a binding adds to a matching request. The spec models this as a oneOf with a type
// discriminator; one struct with the union of the fields is how the SDK sends it, and the fields
// that do not belong to a type are refused rather than ignored.
type Auth struct {
	Type          string         `json:"type"`
	Credential    string         `json:"credential,omitempty"`
	Name          string         `json:"name,omitempty"`
	Headers       []HeaderEntry  `json:"headers,omitempty"`
	Substitutions []Substitution `json:"substitutions,omitempty"`
}

// Binding is one rule: which requests, and what to add to them.
type Binding struct {
	Name  string `json:"name"`
	Match Match  `json:"match"`
	Auth  Auth   `json:"auth"`
}

// CreateRequest is POST /credential-vault.
type CreateRequest struct {
	Credentials []Credential `json:"credentials"`
	Bindings    []Binding    `json:"bindings"`
}

// CredentialMutations is the add/replace/delete set for credentials in a PATCH.
type CredentialMutations struct {
	Add     []Credential `json:"add,omitempty"`
	Replace []Credential `json:"replace,omitempty"`
	Delete  []string     `json:"delete,omitempty"`
}

// BindingMutations is the same for bindings.
type BindingMutations struct {
	Add     []Binding `json:"add,omitempty"`
	Replace []Binding `json:"replace,omitempty"`
	Delete  []string  `json:"delete,omitempty"`
}

// MutationRequest is PATCH /credential-vault, applied whole or not at all.
type MutationRequest struct {
	// ExpectedRevision is the optimistic concurrency guard: the revision the caller read. A
	// mismatch is a conflict, so two writers cannot silently drop each other's bindings.
	ExpectedRevision *int64               `json:"expectedRevision,omitempty"`
	Credentials      *CredentialMutations `json:"credentials,omitempty"`
	Bindings         *BindingMutations    `json:"bindings,omitempty"`
}

// Metadata is a credential as the API reports it: the name, the kind of source, and the revision
// it was last written in. Never the value.
type Metadata struct {
	Name       string `json:"name"`
	SourceType string `json:"sourceType"`
	Revision   int64  `json:"revision"`
}

// AuthMetadata is a binding's auth with the credential names removed - the type, and for apiKey
// the header name, which is not a secret and is what a caller checks a binding by.
type AuthMetadata struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// BindingMetadata is a binding as the API reports it.
type BindingMetadata struct {
	Name     string        `json:"name"`
	Revision int64         `json:"revision"`
	Match    *Match        `json:"match,omitempty"`
	Auth     *AuthMetadata `json:"auth,omitempty"`
}

// State is the sanitized vault: GET /credential-vault, and the body of a successful write.
type State struct {
	Revision    int64             `json:"revision"`
	Credentials []Metadata        `json:"credentials"`
	Bindings    []BindingMetadata `json:"bindings"`
}

// CredentialList is GET /credential-vault/credentials.
type CredentialList struct {
	Revision    int64      `json:"revision"`
	Credentials []Metadata `json:"credentials"`
}

// BindingList is GET /credential-vault/bindings.
type BindingList struct {
	Revision int64             `json:"revision"`
	Bindings []BindingMetadata `json:"bindings"`
}

// The four ways a call fails, told apart because each needs a different answer and a different
// status: a request the vault cannot accept, a vault that is not there, one that is already there,
// and a host that cannot run a credential proxy at all.
var (
	// ErrNotFound is any call on a sandbox with no vault.
	ErrNotFound = errors.New("this sandbox has no credential vault: create one with " +
		"POST /credential-vault")

	// ErrExists is POST on a sandbox that already has one.
	ErrExists = errors.New("this sandbox already has a credential vault: change it with " +
		"PATCH /credential-vault, or remove it with DELETE first")
)

// Error is a request the vault refuses: a malformed binding, an unknown credential, a host the
// egress policy denies. It carries the sentence a caller acts on.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func invalid(format string, a ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, a...)}
}

// RevisionError is a PATCH whose expectedRevision is not the revision in force. The caller re-reads
// and retries, which is what the guard is for.
type RevisionError struct{ Expected, Current int64 }

func (e *RevisionError) Error() string {
	return fmt.Sprintf("expectedRevision %d is not the current revision %d: another writer changed "+
		"this vault - GET /credential-vault and send the PATCH again against the revision it reports",
		e.Expected, e.Current)
}

// NotReadyError is a host that cannot hold a credential vault. It is not a bad request and not a
// missing one: the sandbox exists, the request is well formed, and the machine cannot do this.
type NotReadyError struct{ Message string }

func (e *NotReadyError) Error() string { return e.Message }

// HTTPStatus maps a vault error to the status the Credential Vault API answers with
// (specs/egress-api.yaml): 400 for a request the vault refuses, 404 for no vault, 409 for a
// conflict with the revision in force, and 412 for a credential proxy that cannot be ready here.
func HTTPStatus(err error) int {
	var (
		bad  *Error
		rev  *RevisionError
		cold *NotReadyError
	)

	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &cold):
		return http.StatusPreconditionFailed
	case errors.As(err, &rev):
		return http.StatusConflict
	case errors.As(err, &bad):
		return http.StatusBadRequest
	case errors.Is(err, ErrExists):
		return http.StatusConflict
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	}

	return http.StatusInternalServerError
}
