package osbclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Sandbox states named by the lifecycle spec. The spec says new values may appear, so these
// are for comparison, not an exhaustive enum.
const (
	StatePending    = "Pending"
	StateRunning    = "Running"
	StatePausing    = "Pausing"
	StatePaused     = "Paused"
	StateResuming   = "Resuming"
	StateStopping   = "Stopping"
	StateTerminated = "Terminated"
	StateFailed     = "Failed"
)

// ImageAuth is registry credentials for a private image.
type ImageAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// ImageSpec names the image a sandbox runs.
type ImageSpec struct {
	URI  string     `json:"uri"`
	Auth *ImageAuth `json:"auth,omitempty"`
}

// PlatformSpec pins the image platform.
type PlatformSpec struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// NetworkRule is one egress rule; Target is an FQDN or a leftmost wildcard.
type NetworkRule struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

// NetworkPolicy is the egress policy applied at create.
type NetworkPolicy struct {
	DefaultAction string        `json:"defaultAction,omitempty"`
	Egress        []NetworkRule `json:"egress,omitempty"`
}

// Status is where a sandbox is in its lifecycle, and why.
type Status struct {
	State            string     `json:"state"`
	Reason           string     `json:"reason,omitempty"`
	Message          string     `json:"message,omitempty"`
	LastTransitionAt *time.Time `json:"lastTransitionAt,omitempty"`
}

// Allocation is present when a sandbox came from a server-side pool.
type Allocation struct {
	Mode    string `json:"mode"`
	PoolRef string `json:"poolRef"`
	State   string `json:"state"`
}

// Sandbox is the lifecycle API's view of one sandbox. The create answer carries a subset.
type Sandbox struct {
	ID         string            `json:"id"`
	Image      *ImageSpec        `json:"image,omitempty"`
	SnapshotID string            `json:"snapshotId,omitempty"`
	Platform   *PlatformSpec     `json:"platform,omitempty"`
	Status     Status            `json:"status"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Extensions map[string]string `json:"extensions,omitempty"`
	Allocation *Allocation       `json:"allocation,omitempty"`
	Entrypoint []string          `json:"entrypoint"`
	ExpiresAt  *time.Time        `json:"expiresAt,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
}

// CreateRequest is the body of POST /sandboxes. Exactly one of Image or SnapshotID is a
// startup source. Timeout is seconds (at least 60 on upstream); nil means no expiry.
type CreateRequest struct {
	Image          *ImageSpec        `json:"image,omitempty"`
	SnapshotID     string            `json:"snapshotId,omitempty"`
	Platform       *PlatformSpec     `json:"platform,omitempty"`
	Timeout        *int              `json:"timeout"`
	ResourceLimits map[string]string `json:"resourceLimits,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Entrypoint     []string          `json:"entrypoint,omitempty"`
	NetworkPolicy  *NetworkPolicy    `json:"networkPolicy,omitempty"`
	Extensions     map[string]string `json:"extensions,omitempty"`
}

// Pagination is the page envelope of a list answer.
type Pagination struct {
	Page        int  `json:"page"`
	PageSize    int  `json:"pageSize"`
	TotalItems  int  `json:"totalItems"`
	TotalPages  int  `json:"totalPages"`
	HasNextPage bool `json:"hasNextPage"`
}

// SandboxList is one page of GET /sandboxes.
type SandboxList struct {
	Items      []Sandbox  `json:"items"`
	Pagination Pagination `json:"pagination"`
}

// ListOptions filters GET /sandboxes. States are ORed; everything else is ANDed. Zero values
// are left out of the query, so the server's defaults apply.
type ListOptions struct {
	States   []string
	Metadata map[string]string
	Page     int
	PageSize int
}

// Endpoint is how to reach one port of a sandbox: a URL (possibly without a scheme, which the
// upstream SDKs read as the lifecycle server's own) and headers every request must carry.
type Endpoint struct {
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers,omitempty"`
	// Origin is the Open-Sandbox-Origin header of the answer, when the server sent one.
	Origin string `json:"-"`
}

// CreateSandbox asks for a new sandbox. The answer usually says Pending; WaitReady is what
// turns that into something you can run a command in.
func (c *Client) CreateSandbox(ctx context.Context, req CreateRequest) (*Sandbox, error) {
	var out Sandbox
	if _, err := c.requester().do(ctx, "POST", "/sandboxes", nil, req, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// GetSandbox reads one sandbox.
func (c *Client) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	var out Sandbox
	if _, err := c.requester().do(ctx, "GET", "/sandboxes/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// ListSandboxes reads one page of sandboxes.
func (c *Client) ListSandboxes(ctx context.Context, opts ListOptions) (*SandboxList, error) {
	q := url.Values{}
	for _, s := range opts.States {
		q.Add("state", s)
	}

	if len(opts.Metadata) > 0 {
		q.Set("metadata", EncodeMetadataFilter(opts.Metadata))
	}

	if opts.Page > 0 {
		q.Set("page", strconv.Itoa(opts.Page))
	}

	if opts.PageSize > 0 {
		q.Set("pageSize", strconv.Itoa(opts.PageSize))
	}

	var out SandboxList
	if _, err := c.requester().do(ctx, "GET", "/sandboxes", q, nil, &out); err != nil {
		return nil, err
	}

	if out.Items == nil {
		out.Items = []Sandbox{}
	}

	return &out, nil
}

// EncodeMetadataFilter builds the value of the `metadata` query parameter: `k=v&k2=v2`, each
// key and value percent-encoded once. The query encoder encodes the whole value again, and the
// server undoes one layer before splitting - so a key or value holding `&`, `=` or `%`
// survives, which is what upstream's SDKs do too.
func EncodeMetadataFilter(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, quote(k)+"="+quote(v))
	}

	// Map order is random; a stable query is easier to read in a server log and to test.
	slices.Sort(parts)

	return strings.Join(parts, "&")
}

// DeleteSandbox terminates a sandbox.
func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	_, err := c.requester().do(ctx, "DELETE", "/sandboxes/"+url.PathEscape(id), nil, nil, nil)

	return err
}

// PauseSandbox asks for a pause. It is asynchronous: the sandbox goes Pausing, then Paused.
func (c *Client) PauseSandbox(ctx context.Context, id string) error {
	_, err := c.requester().do(ctx, "POST", "/sandboxes/"+url.PathEscape(id)+"/pause", nil, nil, nil)

	return err
}

// ResumeSandbox asks for a resume. It is asynchronous: Resuming, then Running.
func (c *Client) ResumeSandbox(ctx context.Context, id string) error {
	_, err := c.requester().do(ctx, "POST", "/sandboxes/"+url.PathEscape(id)+"/resume", nil, nil, nil)

	return err
}

// RenewExpiration sets a sandbox's absolute expiry. The contract takes a time, not a
// duration; callers that think in "another ten minutes" add it to time.Now themselves, as the
// upstream SDKs do.
func (c *Client) RenewExpiration(ctx context.Context, id string, at time.Time) (time.Time, error) {
	body := map[string]string{"expiresAt": at.UTC().Format(time.RFC3339Nano)}

	var out struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}

	if _, err := c.requester().do(ctx, "POST", "/sandboxes/"+url.PathEscape(id)+"/renew-expiration", nil, body, &out); err != nil {
		return time.Time{}, err
	}

	return out.ExpiresAt, nil
}

// GetEndpoint resolves how to reach port inside a sandbox. useServerProxy asks the server
// for a URL routed through itself, for callers that cannot reach the sandbox directly.
func (c *Client) GetEndpoint(ctx context.Context, id string, port int, useServerProxy bool) (*Endpoint, error) {
	var q url.Values
	if useServerProxy {
		q = url.Values{"use_server_proxy": {"true"}}
	}

	var out Endpoint

	h, err := c.requester().do(ctx, "GET", fmt.Sprintf("/sandboxes/%s/endpoints/%d", url.PathEscape(id), port), q, nil, &out)
	if err != nil {
		return nil, err
	}

	out.Origin = h.Get(OriginHeader)

	return &out, nil
}

// Execd resolves a sandbox's execd endpoint and returns a client for it.
func (c *Client) Execd(ctx context.Context, id string) (*Execd, error) {
	ep, err := c.GetEndpoint(ctx, id, ExecdPort, false)
	if err != nil {
		return nil, err
	}

	return c.execdFor(ep)
}

func (c *Client) execdFor(ep *Endpoint) (*Execd, error) {
	raw := ep.Endpoint

	// The endpoint may come back without a scheme ("127.0.0.1:41234" or "host/proxy/44772");
	// the upstream SDKs prefix the lifecycle server's own protocol, so this does too.
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		scheme := "http"
		if strings.HasPrefix(c.base, "https://") {
			scheme = "https"
		}

		raw = scheme + "://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("opensandbox: the server gave %q as the execd endpoint, which is not a URL", ep.Endpoint)
	}

	h := map[string]string{}
	for k, v := range ep.Headers {
		h[k] = v
	}

	return &Execd{r: &requester{
		base:      strings.TrimRight(u.String(), "/"),
		headers:   h,
		http:      c.http,
		timeout:   c.timeout,
		userAgent: c.userAgent,
	}}, nil
}

// ErrNotReady is wrapped by WaitReady's error when the deadline passes first.
var ErrNotReady = errors.New("sandbox did not become ready")

// WaitReady polls until the sandbox's execd answers /ping, and returns a client for it. It
// fails at once if the sandbox reaches Failed or Terminated, rather than waiting out the
// deadline for something that will never come - and it fails at once on 401/403, which no
// amount of waiting fixes. The context bounds the wait.
func (c *Client) WaitReady(ctx context.Context, id string, interval time.Duration) (*Execd, error) {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}

	var last error

	for {
		ex, err := c.Execd(ctx, id)
		if err == nil {
			if err = ex.Ping(ctx); err == nil {
				return ex, nil
			}
		}

		last = err

		if s := StatusOf(err); s == 401 || s == 403 {
			return nil, err
		}

		// An endpoint that is not there yet is expected while Pending. Look at the state
		// to tell "not yet" from "never".
		if sb, gerr := c.GetSandbox(ctx, id); gerr == nil {
			if sb.Status.State == StateFailed || sb.Status.State == StateTerminated {
				return nil, fmt.Errorf("opensandbox: sandbox %s is %s (%s): %s",
					id, sb.Status.State, sb.Status.Reason, sb.Status.Message)
			}
		} else if StatusOf(gerr) == 404 {
			return nil, gerr
		}

		t := time.NewTimer(interval)

		select {
		case <-ctx.Done():
			t.Stop()

			return nil, fmt.Errorf("opensandbox: %w: %s: last error: %v", ErrNotReady, id, last)
		case <-t.C:
		}
	}
}

// quote percent-encodes everything but unreserved characters, as Python's quote(safe="")
// does. QueryEscape is that except for spaces, which it writes as "+".
func quote(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
