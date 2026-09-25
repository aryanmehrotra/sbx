// Package osbclient speaks the OpenSandbox HTTP contract: the lifecycle API (`/v1/sandboxes`)
// and execd, the per-sandbox agent that runs commands and touches files.
//
// It is a client of the contract, not of sbx. `sbx mcp` uses it, and it works the same
// against `sbx serve --osb-addr` or a real OpenSandbox server, which is the point: an agent's
// tools should not care which one is on the other end.
//
// Written against the specs at the pinned upstream tag (release-1.1.0,
// specs/sandbox-lifecycle.yml and specs/execd-api.yaml) with the upstream Python and Go SDKs as
// the reference where the specs are silent - endpoint resolution, SSE framing, exit-code
// inference. It is shaped to become sbx's public Go package later: no globals, every call takes
// a context, and nothing here knows about MCP or about sbx's own flags.
package osbclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Header names fixed by the contract.
const (
	// APIKeyHeader authenticates every lifecycle call.
	APIKeyHeader = "OPEN-SANDBOX-API-KEY"
	// ExecdTokenHeader authenticates every execd call. The lifecycle API hands its value out in
	// the endpoint response for port 44772; a client never mints it.
	ExecdTokenHeader = "X-EXECD-ACCESS-TOKEN"
	// OriginHeader is set on endpoint responses for template-backed sandboxes.
	OriginHeader = "Open-Sandbox-Origin"
	// ExecdPort is the port execd listens on inside every sandbox.
	ExecdPort = 44772
	// DefaultURL is where upstream's SDKs look when nothing is configured, and where
	// `sbx serve --osb-addr` binds by default.
	DefaultURL = "http://localhost:8080"
	// DefaultRequestTimeout bounds a non-streaming call. Streaming calls (a foreground command)
	// are bounded only by their context, since a build can legitimately run for an hour.
	DefaultRequestTimeout = 30 * time.Second
)

// Client talks to one OpenSandbox lifecycle server. The zero value is not usable; call New.
type Client struct {
	base      string // scheme://host[:port][/prefix]/v1, no trailing slash
	apiKey    string
	http      *http.Client
	timeout   time.Duration
	userAgent string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the transport. Its Timeout should be zero: streaming calls outlive
// any fixed timeout, and non-streaming calls get theirs from WithRequestTimeout instead.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithRequestTimeout bounds each non-streaming call. Zero means only the context bounds it.
func WithRequestTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithUserAgent sets the User-Agent sent on every call.
func WithUserAgent(ua string) Option { return func(c *Client) { c.userAgent = ua } }

// New returns a client for the server at addr, which may be a full URL
// ("https://osb.example.com"), a bare host ("localhost:8080", taken as http - the upstream SDKs'
// default protocol), or either with the "/v1" suffix already on it.
func New(addr, apiKey string, opts ...Option) (*Client, error) {
	base, err := normalizeBase(addr)
	if err != nil {
		return nil, err
	}

	c := &Client{
		base:      base,
		apiKey:    apiKey,
		http:      &http.Client{},
		timeout:   DefaultRequestTimeout,
		userAgent: "sbx-osbclient",
	}

	for _, o := range opts {
		o(c)
	}

	return c, nil
}

// BaseURL is the lifecycle API root this client calls, ending in /v1.
func (c *Client) BaseURL() string { return c.base }

func normalizeBase(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = DefaultURL
	}

	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}

	u, err := url.Parse(addr)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("opensandbox: %q is not a server address; want http(s)://host:port, e.g. %s",
			addr, DefaultURL)
	}

	u.RawQuery, u.Fragment = "", ""
	p := strings.TrimRight(u.Path, "/")

	// The lifecycle API is versioned in the path. Accept the address with or without it, so a
	// URL copied from the spec's `servers:` block works as well as one copied from a browser.
	if !strings.HasSuffix(p, "/v1") {
		p += "/v1"
	}

	u.Path = p

	return u.String(), nil
}

// APIError is a non-2xx answer from either API. Code and Message come from the contract's
// `{code, message}` body when the server sent one.
type APIError struct {
	Method, Path string
	Status       int
	Code         string
	Message      string
	RequestID    string
}

func (e *APIError) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b, "opensandbox: %s %s: %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))

	if e.Code != "" {
		fmt.Fprintf(&b, " (%s)", e.Code)
	}

	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}

	if e.RequestID != "" {
		fmt.Fprintf(&b, " [request %s]", e.RequestID)
	}

	return b.String()
}

// StatusOf reports the HTTP status of err if it is (or wraps) an APIError, else 0.
func StatusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}

	return 0
}

// maxErrorBody caps how much of an error body is kept. A proxy's HTML error page is not a
// message anyone wants in their tool output.
const maxErrorBody = 2048

func apiError(resp *http.Response, method, path string) *APIError {
	e := &APIError{
		Method: method, Path: path, Status: resp.StatusCode,
		RequestID: resp.Header.Get("X-Request-ID"),
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))

	var shaped struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}

	if json.Unmarshal(body, &shaped) == nil && (shaped.Code != "" || shaped.Message != "") {
		e.Code, e.Message = shaped.Code, shaped.Message
	} else {
		e.Message = strings.TrimSpace(string(body))
	}

	return e
}

// requester is the shared half of Client and Execd: a base URL, headers, and a transport.
type requester struct {
	base      string
	headers   map[string]string
	http      *http.Client
	timeout   time.Duration
	userAgent string
}

// do sends one non-streaming request and decodes a JSON answer into out (nil to discard).
// body, when not nil and not an io.Reader, is sent as JSON.
func (r *requester) do(ctx context.Context, method, path string, query url.Values, body, out any) (http.Header, error) {
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)

		defer cancel()
	}

	resp, err := r.send(ctx, method, path, query, body, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)

		return resp.Header, nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("opensandbox: %s %s: reading answer: %w", method, path, err)
	}

	// Some endpoints answer 200 with an empty body where the spec allows one; that is not
	// malformed JSON, it is nothing to decode.
	if len(bytes.TrimSpace(data)) == 0 {
		return resp.Header, nil
	}

	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("opensandbox: %s %s: answer is not the JSON the contract describes: %w", method, path, err)
	}

	return resp.Header, nil
}

// send issues a request and returns the response if it is 2xx, else an *APIError. The caller
// closes the body.
func (r *requester) send(ctx context.Context, method, path string, query url.Values, body any, hdr http.Header) (*http.Response, error) {
	u := r.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var (
		rd          io.Reader
		contentType string
	)

	switch b := body.(type) {
	case nil:
	case io.Reader:
		rd = b
	default:
		data, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("opensandbox: %s %s: encoding request: %w", method, path, err)
		}

		rd, contentType = bytes.NewReader(data), "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("opensandbox: %s %s: %w", method, path, err)
	}

	for k, v := range r.headers {
		req.Header.Set(k, v)
	}

	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}

	if r.userAgent != "" {
		req.Header.Set("User-Agent", r.userAgent)
	}

	resp, err := r.http.Do(req)
	if err != nil {
		return nil, &TransportError{Method: method, URL: r.base + path, Err: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()

		return nil, apiError(resp, method, path)
	}

	return resp, nil
}

// TransportError is a request that never got an HTTP answer: nothing listening, DNS, TLS,
// or the context ending first.
type TransportError struct {
	Method, URL string
	Err         error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("opensandbox: %s %s: %v", e.Method, e.URL, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

func (c *Client) requester() *requester {
	h := map[string]string{"Accept": "application/json"}
	if c.apiKey != "" {
		h[APIKeyHeader] = c.apiKey
	}

	return &requester{base: c.base, headers: h, http: c.http, timeout: c.timeout, userAgent: c.userAgent}
}
