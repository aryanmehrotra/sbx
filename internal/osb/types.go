package osb

// The wire shapes, field for field from specs/sandbox-lifecycle.yml and specs/diagnostic-api.yml
// at OpenSandbox release-1.1.0. The JSON names are the contract; the Go names are ours.

import (
	"encoding/json"
	"time"
)

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type platformSpec struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type imageSpec struct {
	URI  string          `json:"uri"`
	Auth json.RawMessage `json:"auth,omitempty"`
}

type statusJSON struct {
	State            string     `json:"state"`
	Reason           string     `json:"reason,omitempty"`
	Message          string     `json:"message,omitempty"`
	LastTransitionAt *time.Time `json:"lastTransitionAt,omitempty"`
}

// sandboxJSON is both Sandbox and CreateSandboxResponse: the create response is the same object
// without `image`, which the spec says is only returned by GET.
type sandboxJSON struct {
	ID         string            `json:"id"`
	Image      *imageSpec        `json:"image,omitempty"`
	Status     statusJSON        `json:"status"`
	Metadata   map[string]string `json:"metadata"`
	Extensions map[string]string `json:"extensions,omitempty"`
	Platform   *platformSpec     `json:"platform,omitempty"`
	Entrypoint []string          `json:"entrypoint"`
	ExpiresAt  *time.Time        `json:"expiresAt,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
}

type paginationJSON struct {
	Page        int  `json:"page"`
	PageSize    int  `json:"pageSize"`
	TotalItems  int  `json:"totalItems"`
	TotalPages  int  `json:"totalPages"`
	HasNextPage bool `json:"hasNextPage"`
}

type listJSON struct {
	Items      []sandboxJSON  `json:"items"`
	Pagination paginationJSON `json:"pagination"`
}

// createRequest keeps the fields sbx does not implement as raw JSON, so that their PRESENCE can
// be refused precisely - "networkPolicy arrives in v0.10.0" - rather than decoded, ignored, and
// reported as a success that did not do what was asked.
type createRequest struct {
	Image            *imageSpec        `json:"image"`
	SnapshotID       string            `json:"snapshotId"`
	TemplateID       string            `json:"templateId"`
	Platform         *platformSpec     `json:"platform"`
	Timeout          *int              `json:"timeout"`
	ResourceLimits   map[string]string `json:"resourceLimits"`
	ResourceRequests map[string]string `json:"resourceRequests"`
	Env              map[string]string `json:"env"`
	Metadata         map[string]string `json:"metadata"`
	Lifecycle        json.RawMessage   `json:"lifecycle"`
	Entrypoint       []string          `json:"entrypoint"`
	NetworkPolicy    json.RawMessage   `json:"networkPolicy"`
	CredentialProxy  json.RawMessage   `json:"credentialProxy"`
	SecureAccess     bool              `json:"secureAccess"`
	Volumes          []json.RawMessage `json:"volumes"`
	Extensions       map[string]string `json:"extensions"`
}

type renewRequest struct {
	ExpiresAt *time.Time `json:"expiresAt"`
}

type renewResponse struct {
	ExpiresAt time.Time `json:"expiresAt"`
}

type endpointJSON struct {
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers,omitempty"`
}

type metricsEvent struct {
	EventType        string `json:"eventType"`
	SandboxID        string `json:"sandboxId"`
	Image            string `json:"image"`
	CreateDurationMs *int64 `json:"createDurationMs"`
	Success          *bool  `json:"success"`
}

type diagnosticJSON struct {
	SandboxID     string   `json:"sandboxId"`
	Kind          string   `json:"kind"`
	Scope         string   `json:"scope"`
	Delivery      string   `json:"delivery"`
	ContentType   string   `json:"contentType"`
	Content       string   `json:"content"`
	ContentLength int      `json:"contentLength"`
	Truncated     bool     `json:"truncated"`
	Warnings      []string `json:"warnings,omitempty"`
}
