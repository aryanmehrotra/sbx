package mcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aryanmehrotra/sbx/internal/osbclient"
)

// Instructions is what the server tells the model about itself at initialize.
const Instructions = "Use these tools to create and manage isolated sandboxes. Keep the sandbox_id " +
	"returned by sandbox_create or sandbox_connect and pass it to every other call. Use command_run to " +
	"execute, file_read/file_write for file IO, sandbox_get_endpoint to reach a port inside the sandbox, " +
	"and sandbox_kill when you are finished - a sandbox you do not kill lives until its timeout. " +
	"For large files, read in slices with range_header."

// Sandboxes is the tool set: upstream OpenSandbox MCP's nineteen tools, over an osbclient.
type Sandboxes struct {
	client *osbclient.Client
	// url is only for error messages: which server could not be reached.
	url string

	mu    sync.Mutex
	execd map[string]*osbclient.Execd
}

// NewSandboxes returns the tool set for a lifecycle server.
func NewSandboxes(c *osbclient.Client) *Sandboxes {
	return &Sandboxes{client: c, url: strings.TrimSuffix(c.BaseURL(), "/v1"), execd: map[string]*osbclient.Execd{}}
}

// execdFor returns a cached execd client for a sandbox, resolving it on first use.
//
// Upstream's server keeps a registry and refuses a sandbox_id it has not seen unless the
// caller passes connect_if_missing. That turns "list, then run a command in one of them" into
// three calls and an error, for no safety gain - the id is the capability either way - so sbx
// resolves on demand and treats the registry as the cache it really is.
func (s *Sandboxes) execdFor(ctx context.Context, id string) (*osbclient.Execd, error) {
	s.mu.Lock()
	ex := s.execd[id]
	s.mu.Unlock()

	if ex != nil {
		return ex, nil
	}

	ex, err := s.client.Execd(ctx, id)
	if err != nil {
		return nil, s.explain(err)
	}

	s.remember(id, ex)

	return ex, nil
}

func (s *Sandboxes) remember(id string, ex *osbclient.Execd) {
	s.mu.Lock()
	s.execd[id] = ex
	s.mu.Unlock()
}

func (s *Sandboxes) forget(id string) {
	s.mu.Lock()
	delete(s.execd, id)
	s.mu.Unlock()
}

// execdErr turns an execd failure into a tool error, and drops the cached endpoint when the
// failure means it may be stale - nothing answered, or the token was refused - so the next
// call resolves afresh. It does not retry: a POST that failed in transit may have run.
func (s *Sandboxes) execdErr(id string, err error) error {
	var te *osbclient.TransportError
	if errors.As(err, &te) || osbclient.StatusOf(err) == 401 {
		s.forget(id)

		return fmt.Errorf("%w (the sandbox's execd did not accept the call; the endpoint has been "+
			"re-resolved for the next one - check sandbox_get_info for its state, then retry)", err)
	}

	return err
}

// explain adds what to do next to a lifecycle failure.
func (s *Sandboxes) explain(err error) error {
	var te *osbclient.TransportError
	if errors.As(err, &te) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w\ncannot reach the OpenSandbox server at %s: start one with "+
			"`sbx serve --osb-addr 127.0.0.1:8080`, or point this server elsewhere with "+
			"`sbx mcp --url` / SBX_OSB_URL", err, s.url)
	}

	switch osbclient.StatusOf(err) {
	case 401, 403:
		return fmt.Errorf("%w\nthe server refused the API key: start `sbx mcp` with --key or "+
			"SBX_OSB_KEY set to the key the server was started with (a local sbx serve with no --osb-key "+
			"generated one in ~/.sbx/osb/key)", err)
	case 404:
		return fmt.Errorf("%w\nno such sandbox: sandbox_list shows the ones that exist", err)
	}

	return err
}

// Tools returns every tool, ready to Register.
func (s *Sandboxes) Tools() []Tool {
	readOnly := &Annotations{ReadOnlyHint: ptr(true), OpenWorldHint: ptr(false)}
	destructive := &Annotations{ReadOnlyHint: ptr(false), DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}
	additive := &Annotations{ReadOnlyHint: ptr(false), DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}

	return []Tool{
		{
			Name: "sandbox_create", Title: "Create sandbox", Annotations: additive,
			Description: "Create a sandbox from a container image and wait until it can run commands. " +
				"Returns {sandbox_id, info}; keep sandbox_id for every later call. The sandbox expires " +
				"after timeout_seconds unless renewed (sandbox_renew) and should be removed with " +
				"sandbox_kill when finished.",
			InputSchema: object(map[string]schema{
				"image":                            str(`Container image reference, e.g. "python:3.12" or "node:22".`),
				"auth_username":                    optional(str("Registry username, for a private image.")),
				"auth_password":                    optional(str("Registry password or token, for a private image.")),
				"timeout_seconds":                  withDefault(number("Sandbox lifetime in seconds from now (at least 60)."), 600),
				"ready_timeout_seconds":            withDefault(number("How long to wait for the sandbox to accept commands."), 30),
				"health_check_polling_interval_ms": withDefault(integer("Interval between readiness checks, in ms."), 200),
				"skip_health_check":                withDefault(boolean("Return without waiting for readiness."), false),
				"env":                              optional(stringMap("Environment variables for the sandbox.")),
				"metadata":                         optional(stringMap("Labels to attach, filterable in sandbox_list.")),
				"resource":                         optional(stringMap(`Resource limits, e.g. {"cpu": "1", "memory": "2Gi"} (the default).`)),
				"network_policy":                   optional(networkPolicySchema()),
				"extensions":                       optional(stringMap("Opaque server-specific options, passed through.")),
				"entrypoint":                       optional(arrayOf(str(""), `Entrypoint command; defaults to ["tail", "-f", "/dev/null"].`)),
			}, "image"),
			Handler: s.create,
		},
		{
			Name: "sandbox_connect", Title: "Connect to sandbox", Annotations: readOnly,
			Description: "Attach to an existing sandbox and wait until it can run commands. Returns " +
				"{sandbox_id, info}. Optional in sbx: every tool resolves a sandbox_id on demand.",
			InputSchema: object(map[string]schema{
				"sandbox_id":                       sandboxID(),
				"connect_timeout_seconds":          withDefault(number("How long to wait for readiness."), 30),
				"health_check_polling_interval_ms": withDefault(integer("Interval between readiness checks, in ms."), 200),
				"skip_health_check":                withDefault(boolean("Return without waiting for readiness."), false),
			}, "sandbox_id"),
			Handler: s.connect,
		},
		{
			Name: "sandbox_kill", Title: "Kill sandbox", Annotations: destructive,
			Description: `Terminate a sandbox and delete everything in it. Returns {"status": "killed"}.`,
			InputSchema: object(map[string]schema{"sandbox_id": sandboxID()}, "sandbox_id"),
			Handler:     s.kill,
		},
		{
			Name: "sandbox_get_info", Title: "Get sandbox info", Annotations: readOnly,
			Description: "Read a sandbox's state, image, metadata and expiry.",
			InputSchema: object(map[string]schema{"sandbox_id": sandboxID()}, "sandbox_id"),
			Handler:     s.getInfo,
		},
		{
			Name: "sandbox_list", Title: "List sandboxes", Annotations: readOnly,
			Description: "List sandboxes, optionally filtered by state or metadata. Returns " +
				"{sandbox_infos, pagination}.",
			InputSchema: object(map[string]schema{
				"filter": optional(object(map[string]schema{
					"states":    optional(arrayOf(str(""), `States to include, ORed: "Pending", "Running", "Paused", ...`)),
					"metadata":  optional(stringMap("Metadata that must all match.")),
					"page_size": optional(integer("Items per page.")),
					"page":      optional(integer("Page number, from 1.")),
				})),
			}),
			Handler: s.list,
		},
		{
			Name: "sandbox_renew", Title: "Renew sandbox", Annotations: additive,
			Description: "Push a sandbox's expiry out: it will now expire timeout_seconds from now. " +
				"Returns {expires_at}.",
			InputSchema: object(map[string]schema{
				"sandbox_id":      sandboxID(),
				"timeout_seconds": number("New remaining lifetime, in seconds from now."),
			}, "sandbox_id", "timeout_seconds"),
			Handler: s.renew,
		},
		{
			Name: "sandbox_healthcheck", Title: "Check sandbox health", Annotations: readOnly,
			Description: "Report whether a sandbox answers. Returns {sandbox_id, healthy}.",
			InputSchema: object(map[string]schema{"sandbox_id": sandboxID(), "connect_if_missing": connectIfMissing()}, "sandbox_id"),
			Handler:     s.healthcheck,
		},
		{
			Name: "sandbox_get_metrics", Title: "Get sandbox metrics", Annotations: readOnly,
			Description: "Read a sandbox's CPU and memory use.",
			InputSchema: object(map[string]schema{"sandbox_id": sandboxID(), "connect_if_missing": connectIfMissing()}, "sandbox_id"),
			Handler:     s.metrics,
		},
		{
			Name: "sandbox_get_endpoint", Title: "Get sandbox endpoint", Annotations: readOnly,
			Description: "Get the URL (and any headers it needs) that reaches a port inside the sandbox, " +
				"e.g. a web server started with command_run. Returns {endpoint, headers, origin}.",
			InputSchema: object(map[string]schema{
				"sandbox_id":         sandboxID(),
				"port":               integer("Port the service listens on inside the sandbox."),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "port"),
			Handler: s.endpoint,
		},
		{
			Name: "command_run", Title: "Run command", Annotations: additive,
			Description: "Run a shell command inside a sandbox (pipes and redirects work). Waits for it " +
				"to finish and returns its execution: id, exit_code, logs.stdout/logs.stderr, error, " +
				"complete. With background=true it returns at once with the id, for command_interrupt.",
			InputSchema: object(map[string]schema{
				"sandbox_id":         sandboxID(),
				"command":            str("Shell command to run."),
				"background":         withDefault(boolean("Start it and return immediately."), false),
				"working_directory":  optional(str("Directory to run it in.")),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "command"),
			Handler: s.run,
		},
		{
			Name: "command_interrupt", Title: "Interrupt command", Annotations: destructive,
			Description: `Interrupt a running command by its execution id. Returns {"status": "interrupted"}.`,
			InputSchema: object(map[string]schema{
				"sandbox_id":         sandboxID(),
				"execution_id":       str("The id from command_run's result."),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "execution_id"),
			Handler: s.interrupt,
		},
		{
			Name: "file_read", Title: "Read file", Annotations: readOnly,
			Description: `Read a text file from the sandbox. Returns {path, content}. For a large file pass ` +
				`range_header, e.g. "bytes=0-65535", and read it in slices.`,
			InputSchema: object(map[string]schema{
				"sandbox_id":         sandboxID(),
				"path":               str("File path inside the sandbox."),
				"encoding":           withDefault(str(`Text encoding: "utf-8" or "latin-1".`), "utf-8"),
				"range_header":       optional(str(`HTTP byte range, e.g. "bytes=0-1023".`)),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "path"),
			Handler: s.fileRead,
		},
		{
			Name: "file_write", Title: "Write file", Annotations: additive,
			Description: `Write a text file inside the sandbox, creating or replacing it. Returns {"status": "written"}.`,
			InputSchema: object(map[string]schema{
				"sandbox_id":         sandboxID(),
				"path":               str("Destination path inside the sandbox."),
				"content":            str("File content."),
				"encoding":           withDefault(str(`Text encoding: "utf-8" or "latin-1".`), "utf-8"),
				"mode":               withDefault(integer("Unix permissions written as octal digits, e.g. 644 or 755."), 755),
				"owner":              optional(str("Owner user name.")),
				"group":              optional(str("Group name.")),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "path", "content"),
			Handler: s.fileWrite,
		},
		{
			Name: "file_delete", Title: "Delete files", Annotations: destructive,
			Description: `Delete files inside the sandbox. Returns {"status": "deleted"}.`,
			InputSchema: object(map[string]schema{
				"sandbox_id":         sandboxID(),
				"paths":              arrayOf(str(""), "File paths to delete."),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "paths"),
			Handler: s.fileDelete,
		},
		{
			Name: "file_search", Title: "Search files", Annotations: readOnly,
			Description: "Find files under a directory whose names match a glob. Returns a list of " +
				"entries (path, type, size, mode, owner, group, modified_at, created_at).",
			InputSchema: object(map[string]schema{
				"sandbox_id":         sandboxID(),
				"path":               str("Directory to search under."),
				"pattern":            str(`Glob, e.g. "*.py".`),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "path", "pattern"),
			Handler: s.fileSearch,
		},
		{
			Name: "file_create_directories", Title: "Create directories", Annotations: additive,
			Description: `Create directories, with parents, like mkdir -p. Returns {"status": "created"}.`,
			InputSchema: object(map[string]schema{
				"sandbox_id": sandboxID(),
				"entries": arrayOf(object(map[string]schema{
					"path":  str("Directory path."),
					"mode":  withDefault(integer("Unix permissions written as octal digits."), 755),
					"owner": optional(str("Owner user name.")),
					"group": optional(str("Group name.")),
				}, "path"), "Directories to create."),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "entries"),
			Handler: s.mkdirs,
		},
		{
			Name: "file_delete_directories", Title: "Delete directories", Annotations: destructive,
			Description: `Delete directories and everything in them, like rm -rf. Returns {"status": "deleted"}.`,
			InputSchema: object(map[string]schema{
				"sandbox_id":         sandboxID(),
				"paths":              arrayOf(str(""), "Directory paths to delete."),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "paths"),
			Handler: s.rmdirs,
		},
		{
			Name: "file_move", Title: "Move files", Annotations: destructive,
			Description: `Move or rename files and directories, in order. Returns {"status": "moved"}.`,
			InputSchema: object(map[string]schema{
				"sandbox_id": sandboxID(),
				"entries": arrayOf(object(map[string]schema{
					"source":      str("Current path."),
					"destination": str("New path."),
				}, "source", "destination"), "Moves to make."),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "entries"),
			Handler: s.move,
		},
		{
			Name: "file_replace_contents", Title: "Replace in files", Annotations: destructive,
			Description: "Replace every occurrence of old_content with new_content in each file. Returns " +
				"[{path, replaced_count}]; 0 means old_content was not found.",
			InputSchema: object(map[string]schema{
				"sandbox_id": sandboxID(),
				"entries": arrayOf(object(map[string]schema{
					"path":        str("File to edit."),
					"old_content": str("Exact text to find."),
					"new_content": str("Text to put in its place."),
				}, "path", "old_content", "new_content"), "Edits to make."),
				"connect_if_missing": connectIfMissing(),
			}, "sandbox_id", "entries"),
			Handler: s.replace,
		},
	}
}

func networkPolicySchema() schema {
	return object(map[string]schema{
		"defaultAction": optional(schema{"type": "string", "enum": []string{"allow", "deny"},
			"description": "What happens to traffic no rule matches."}),
		"egress": optional(arrayOf(object(map[string]schema{
			"action": schema{"type": "string", "enum": []string{"allow", "deny"}},
			"target": str(`FQDN or wildcard, e.g. "pypi.org" or "*.github.com".`),
		}, "action", "target"), "Egress rules.")),
	})
}

// --- outputs, in the upstream Python SDK's snake_case serialization ---

type sandboxInfo struct {
	ID         string            `json:"id"`
	Status     statusOut         `json:"status"`
	Entrypoint []string          `json:"entrypoint"`
	ExpiresAt  *string           `json:"expires_at"`
	CreatedAt  string            `json:"created_at"`
	Image      *imageOut         `json:"image"`
	SnapshotID *string           `json:"snapshot_id"`
	Platform   *platformOut      `json:"platform"`
	Allocation *allocationOut    `json:"allocation"`
	Metadata   map[string]string `json:"metadata"`
	Extensions map[string]string `json:"extensions"`
}

type statusOut struct {
	State            string  `json:"state"`
	Reason           *string `json:"reason"`
	Message          *string `json:"message"`
	LastTransitionAt *string `json:"last_transition_at"`
}

type imageOut struct {
	Image string `json:"image"`
	Auth  any    `json:"auth"`
}

type platformOut struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type allocationOut struct {
	Mode    string `json:"mode"`
	PoolRef string `json:"pool_ref"`
	State   string `json:"state"`
}

type sandboxResult struct {
	SandboxID string      `json:"sandbox_id"`
	Info      sandboxInfo `json:"info"`
}

type statusResult struct {
	Status string `json:"status"`
}

func timeStr(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func optTime(t *time.Time) *string {
	if t == nil {
		return nil
	}

	s := timeStr(*t)

	return &s
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

func toInfo(sb *osbclient.Sandbox) sandboxInfo {
	out := sandboxInfo{
		ID: sb.ID,
		Status: statusOut{
			State: sb.Status.State, Reason: optStr(sb.Status.Reason), Message: optStr(sb.Status.Message),
			LastTransitionAt: optTime(sb.Status.LastTransitionAt),
		},
		Entrypoint: sb.Entrypoint,
		ExpiresAt:  optTime(sb.ExpiresAt),
		CreatedAt:  timeStr(sb.CreatedAt),
		SnapshotID: optStr(sb.SnapshotID),
		Metadata:   sb.Metadata,
		Extensions: sb.Extensions,
	}

	if out.Entrypoint == nil {
		out.Entrypoint = []string{}
	}

	if sb.Image != nil {
		out.Image = &imageOut{Image: sb.Image.URI}
	}

	if sb.Platform != nil {
		out.Platform = &platformOut{OS: sb.Platform.OS, Arch: sb.Platform.Arch}
	}

	if sb.Allocation != nil {
		out.Allocation = &allocationOut{Mode: sb.Allocation.Mode, PoolRef: sb.Allocation.PoolRef, State: sb.Allocation.State}
	}

	return out
}

// --- handlers ---

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func (s *Sandboxes) create(ctx context.Context, call *Call) (any, error) {
	var a struct {
		Image               string            `json:"image"`
		AuthUsername        *string           `json:"auth_username"`
		AuthPassword        *string           `json:"auth_password"`
		TimeoutSeconds      *float64          `json:"timeout_seconds"`
		ReadyTimeoutSeconds *float64          `json:"ready_timeout_seconds"`
		PollIntervalMs      *int              `json:"health_check_polling_interval_ms"`
		SkipHealthCheck     bool              `json:"skip_health_check"`
		Env                 map[string]string `json:"env"`
		Metadata            map[string]string `json:"metadata"`
		Resource            map[string]string `json:"resource"`
		NetworkPolicy       *networkPolicyIn  `json:"network_policy"`
		Extensions          map[string]string `json:"extensions"`
		Entrypoint          []string          `json:"entrypoint"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	if strings.TrimSpace(a.Image) == "" {
		return nil, errors.New(`image is required, e.g. "python:3.12"`)
	}

	req := osbclient.CreateRequest{
		Image:          &osbclient.ImageSpec{URI: a.Image},
		Env:            a.Env,
		Metadata:       a.Metadata,
		ResourceLimits: a.Resource,
		Extensions:     a.Extensions,
		Entrypoint:     a.Entrypoint,
	}

	u, p := deref(a.AuthUsername), deref(a.AuthPassword)
	if (u == "") != (p == "") {
		return nil, errors.New("auth_username and auth_password must be given together")
	}

	if u != "" {
		req.Image.Auth = &osbclient.ImageAuth{Username: u, Password: p}
	}

	// The defaults upstream's SDK fills in: the contract requires an entrypoint with an
	// image, and a sandbox with none of its own work to do should just stay up.
	if len(req.Entrypoint) == 0 {
		req.Entrypoint = []string{"tail", "-f", "/dev/null"}
	}

	if len(req.ResourceLimits) == 0 {
		req.ResourceLimits = map[string]string{"cpu": "1", "memory": "2Gi"}
	}

	ttl := 600.0
	if a.TimeoutSeconds != nil {
		ttl = *a.TimeoutSeconds
	}

	t := int(math.Ceil(ttl))
	req.Timeout = &t

	if a.NetworkPolicy != nil {
		req.NetworkPolicy = a.NetworkPolicy.policy()
	}

	call.Progress(0.3, 1, "Creating sandbox")

	sb, err := s.client.CreateSandbox(ctx, req)
	if err != nil {
		return nil, s.explain(err)
	}

	if !a.SkipHealthCheck {
		call.Progress(0.5, 1, "Waiting for the sandbox to accept commands")

		if err := s.waitReady(ctx, sb.ID, a.ReadyTimeoutSeconds, a.PollIntervalMs); err != nil {
			// A sandbox the caller never got an id for is one nobody will kill. Upstream's
			// SDKs remove it too.
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()

			if derr := s.client.DeleteSandbox(dctx, sb.ID); derr != nil {
				return nil, fmt.Errorf("%w\nremoving it failed too (%v): kill %s yourself with sandbox_kill", err, derr, sb.ID)
			}

			return nil, fmt.Errorf("%w\nthe sandbox was removed; raise ready_timeout_seconds for a slow image, "+
				"or check the server's logs for why it did not start", err)
		}
	}

	return s.infoResult(ctx, sb.ID, sb)
}

func (s *Sandboxes) waitReady(ctx context.Context, id string, timeoutSec *float64, intervalMs *int) error {
	timeout := 30 * time.Second
	if timeoutSec != nil && *timeoutSec > 0 {
		timeout = seconds(*timeoutSec)
	}

	interval := 200 * time.Millisecond
	if intervalMs != nil && *intervalMs > 0 {
		interval = time.Duration(*intervalMs) * time.Millisecond
	}

	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ex, err := s.client.WaitReady(wctx, id, interval)
	if err != nil {
		return s.explain(err)
	}

	s.remember(id, ex)

	return nil
}

// infoResult reads the sandbox's full info, falling back to what the caller already has when
// the read fails - the sandbox exists and the caller needs its id more than a fresh status.
func (s *Sandboxes) infoResult(ctx context.Context, id string, fallback *osbclient.Sandbox) (any, error) {
	sb, err := s.client.GetSandbox(ctx, id)
	if err != nil {
		if fallback == nil {
			return nil, s.explain(err)
		}

		sb = fallback
	}

	return sandboxResult{SandboxID: sb.ID, Info: toInfo(sb)}, nil
}

type networkPolicyIn struct {
	DefaultAction  *string `json:"defaultAction"`
	DefaultAction2 *string `json:"default_action"`
	Egress         []struct {
		Action string `json:"action"`
		Target string `json:"target"`
	} `json:"egress"`
}

func (n *networkPolicyIn) policy() *osbclient.NetworkPolicy {
	p := &osbclient.NetworkPolicy{DefaultAction: deref(n.DefaultAction)}
	if p.DefaultAction == "" {
		p.DefaultAction = deref(n.DefaultAction2)
	}

	for _, r := range n.Egress {
		p.Egress = append(p.Egress, osbclient.NetworkRule{Action: r.Action, Target: r.Target})
	}

	return p
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}

	return *p
}

func (s *Sandboxes) connect(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID             string   `json:"sandbox_id"`
		ConnectTimeoutSeconds *float64 `json:"connect_timeout_seconds"`
		PollIntervalMs        *int     `json:"health_check_polling_interval_ms"`
		SkipHealthCheck       bool     `json:"skip_health_check"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	if a.SkipHealthCheck {
		if _, err := s.execdFor(ctx, a.SandboxID); err != nil {
			return nil, err
		}
	} else if err := s.waitReady(ctx, a.SandboxID, a.ConnectTimeoutSeconds, a.PollIntervalMs); err != nil {
		return nil, err
	}

	return s.infoResult(ctx, a.SandboxID, nil)
}

type idArgs struct {
	SandboxID        string `json:"sandbox_id"`
	ConnectIfMissing bool   `json:"connect_if_missing"`
}

func (s *Sandboxes) kill(ctx context.Context, call *Call) (any, error) {
	var a idArgs
	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	s.forget(a.SandboxID)

	if err := s.client.DeleteSandbox(ctx, a.SandboxID); err != nil {
		return nil, s.explain(err)
	}

	return statusResult{"killed"}, nil
}

func (s *Sandboxes) getInfo(ctx context.Context, call *Call) (any, error) {
	var a idArgs
	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	sb, err := s.client.GetSandbox(ctx, a.SandboxID)
	if err != nil {
		return nil, s.explain(err)
	}

	return toInfo(sb), nil
}

func (s *Sandboxes) list(ctx context.Context, call *Call) (any, error) {
	var a struct {
		Filter *struct {
			States   []string          `json:"states"`
			Metadata map[string]string `json:"metadata"`
			PageSize *int              `json:"page_size"`
			Page     *int              `json:"page"`
		} `json:"filter"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	var opts osbclient.ListOptions

	if f := a.Filter; f != nil {
		if f.PageSize != nil && *f.PageSize <= 0 {
			return nil, errors.New("filter.page_size must be positive")
		}

		opts = osbclient.ListOptions{States: f.States, Metadata: f.Metadata, PageSize: deref(f.PageSize), Page: deref(f.Page)}
	}

	l, err := s.client.ListSandboxes(ctx, opts)
	if err != nil {
		return nil, s.explain(err)
	}

	infos := make([]sandboxInfo, 0, len(l.Items))
	for i := range l.Items {
		infos = append(infos, toInfo(&l.Items[i]))
	}

	return map[string]any{
		"sandbox_infos": infos,
		"pagination": map[string]any{
			"page": l.Pagination.Page, "page_size": l.Pagination.PageSize, "total_items": l.Pagination.TotalItems,
			"total_pages": l.Pagination.TotalPages, "has_next_page": l.Pagination.HasNextPage,
		},
	}, nil
}

func (s *Sandboxes) renew(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID      string   `json:"sandbox_id"`
		TimeoutSeconds *float64 `json:"timeout_seconds"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	if a.TimeoutSeconds == nil || *a.TimeoutSeconds <= 0 {
		return nil, errors.New("timeout_seconds is required and must be positive: the new lifetime, counted from now")
	}

	// The contract takes an absolute time; "from now" is computed here, as upstream's SDK does.
	at, err := s.client.RenewExpiration(ctx, a.SandboxID, time.Now().Add(seconds(*a.TimeoutSeconds)))
	if err != nil {
		return nil, s.explain(err)
	}

	return map[string]string{"expires_at": timeStr(at)}, nil
}

func (s *Sandboxes) healthcheck(ctx context.Context, call *Call) (any, error) {
	var a idArgs
	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	healthy := false

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		// A sandbox that does not exist is a wrong question, not an unhealthy answer.
		if osbclient.StatusOf(err) == 404 {
			if _, gerr := s.client.GetSandbox(ctx, a.SandboxID); osbclient.StatusOf(gerr) == 404 {
				return nil, err
			}
		}
	} else if perr := ex.Ping(ctx); perr == nil {
		healthy = true
	} else {
		_ = s.execdErr(a.SandboxID, perr)
	}

	return map[string]any{"sandbox_id": a.SandboxID, "healthy": healthy}, nil
}

func (s *Sandboxes) metrics(ctx context.Context, call *Call) (any, error) {
	var a idArgs
	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		return nil, err
	}

	m, err := ex.Metrics(ctx)
	if err != nil {
		return nil, s.execdErr(a.SandboxID, err)
	}

	return map[string]any{
		"cpu_count": m.CPUCount, "cpu_used_percentage": m.CPUUsedPct,
		"memory_total_in_mib": m.MemTotalMiB, "memory_used_in_mib": m.MemUsedMiB,
		"timestamp": m.Timestamp,
	}, nil
}

func (s *Sandboxes) endpoint(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID        string `json:"sandbox_id"`
		Port             int    `json:"port"`
		ConnectIfMissing bool   `json:"connect_if_missing"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	if a.Port < 1 || a.Port > 65535 {
		return nil, fmt.Errorf("port %d is not a TCP port (1-65535)", a.Port)
	}

	ep, err := s.client.GetEndpoint(ctx, a.SandboxID, a.Port, false)
	if err != nil {
		return nil, s.explain(err)
	}

	headers := ep.Headers
	if headers == nil {
		headers = map[string]string{}
	}

	return map[string]any{"endpoint": ep.Endpoint, "headers": headers, "origin": optStr(ep.Origin)}, nil
}

// idWait bounds how long a cancelled command_run keeps its stream open waiting for the command's
// id, so that it can interrupt it. execd sends the id first, so this is only reached when execd
// has stalled - and then there is nothing to interrupt by name anyway.
const idWait = 5 * time.Second

func (s *Sandboxes) run(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID        string  `json:"sandbox_id"`
		Command          string  `json:"command"`
		Background       bool    `json:"background"`
		WorkingDirectory *string `json:"working_directory"`
		ConnectIfMissing bool    `json:"connect_if_missing"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	if strings.TrimSpace(a.Command) == "" {
		return nil, errors.New("command is required")
	}

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		return nil, err
	}

	req := osbclient.CommandRequest{Command: a.Command, Background: a.Background, Cwd: deref(a.WorkingDirectory)}
	x := osbclient.NewExecution()

	var events float64

	// The stream is not tied to ctx. A cancel that arrives after execd has started the command
	// but before its init event has been read would otherwise drop the stream with the id
	// unread, and upstream execd does not stop a command whose reader went away. So on cancel
	// the stream is kept until the id is known (or idWait passes), and only then dropped.
	sctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()

	idKnown := make(chan struct{})
	streamed := make(chan struct{})

	go func() {
		select {
		case <-streamed:
			return
		case <-ctx.Done():
		}

		t := time.NewTimer(idWait)
		defer t.Stop()

		select {
		case <-idKnown:
		case <-streamed:
		case <-t.C:
		}

		stop()
	}()

	err = ex.StreamCommand(sctx, req, func(ev osbclient.Event) error {
		hadID := x.ID != nil
		x.Apply(ev)

		if !hadID && x.ID != nil {
			close(idKnown)
		}

		if ctx.Err() == nil && (ev.Type == "stdout" || ev.Type == "stderr") {
			events++
			call.Progress(events, 0, clipLine(ev.Text))
		}

		return nil
	})

	close(streamed)

	if ctx.Err() != nil && x.ID != nil && !a.Background {
		// The client cancelled. Stopping the command is what cancelling a command means; left
		// alone it would run on in the sandbox with nobody reading it.
		ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		_ = ex.InterruptCommand(ictx, *x.ID)

		return nil, ctx.Err()
	}

	if err != nil {
		return nil, s.execdErr(a.SandboxID, err)
	}

	if !a.Background {
		x.InferExitCode()
	}

	return x, nil
}

func clipLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "..."
	}

	return s
}

func (s *Sandboxes) interrupt(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID        string `json:"sandbox_id"`
		ExecutionID      string `json:"execution_id"`
		ConnectIfMissing bool   `json:"connect_if_missing"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		return nil, err
	}

	if err := ex.InterruptCommand(ctx, a.ExecutionID); err != nil {
		return nil, s.execdErr(a.SandboxID, err)
	}

	return statusResult{"interrupted"}, nil
}

func (s *Sandboxes) fileRead(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID        string  `json:"sandbox_id"`
		Path             string  `json:"path"`
		Encoding         string  `json:"encoding"`
		RangeHeader      *string `json:"range_header"`
		ConnectIfMissing bool    `json:"connect_if_missing"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		return nil, err
	}

	data, err := ex.ReadFile(ctx, a.Path, deref(a.RangeHeader))
	if err != nil {
		return nil, s.execdErr(a.SandboxID, err)
	}

	text, err := decode(data, a.Encoding)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.Path, err)
	}

	return map[string]string{"path": a.Path, "content": text}, nil
}

// decode turns bytes into text. Go's standard library has UTF-8 and nothing else, and Latin-1
// is the one other encoding that is a straight byte-to-rune mapping; anything more is a
// question for iconv inside the sandbox.
func decode(data []byte, enc string) (string, error) {
	switch normEncoding(enc) {
	case "utf-8":
		if !utf8.Valid(data) {
			return "", errors.New(`the file is not valid UTF-8; it may be binary, or a range that splits a character. ` +
				`Try encoding "latin-1", or command_run with base64 or iconv`)
		}

		return string(data), nil
	case "latin-1":
		r := make([]rune, len(data))
		for i, b := range data {
			r[i] = rune(b)
		}

		return string(r), nil
	}

	return "", fmt.Errorf(`encoding %q is not supported here; use "utf-8" or "latin-1", or convert with iconv via command_run`, enc)
}

func encode(text, enc string) ([]byte, error) {
	switch normEncoding(enc) {
	case "utf-8":
		return []byte(text), nil
	case "latin-1":
		out := make([]byte, 0, len(text))

		for _, r := range text {
			if r > 0xff {
				return nil, fmt.Errorf("%q cannot be written as latin-1; use utf-8", r)
			}

			out = append(out, byte(r))
		}

		return out, nil
	}

	return nil, fmt.Errorf(`encoding %q is not supported here; use "utf-8" or "latin-1"`, enc)
}

func normEncoding(enc string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(enc), "_", "-")) {
	case "", "utf-8", "utf8":
		return "utf-8"
	case "latin-1", "latin1", "iso-8859-1", "iso8859-1":
		return "latin-1"
	}

	return enc
}

func (s *Sandboxes) fileWrite(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID        string  `json:"sandbox_id"`
		Path             string  `json:"path"`
		Content          *string `json:"content"`
		Encoding         string  `json:"encoding"`
		Mode             *int    `json:"mode"`
		Owner            *string `json:"owner"`
		Group            *string `json:"group"`
		ConnectIfMissing bool    `json:"connect_if_missing"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	if a.Content == nil {
		return nil, errors.New("content is required (it may be empty)")
	}

	data, err := encode(*a.Content, a.Encoding)
	if err != nil {
		return nil, err
	}

	mode := 755
	if a.Mode != nil {
		mode = *a.Mode
	}

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		return nil, err
	}

	if err := ex.WriteFile(ctx, a.Path, data, osbclient.Permission{Mode: mode, Owner: deref(a.Owner), Group: deref(a.Group)}); err != nil {
		return nil, s.execdErr(a.SandboxID, err)
	}

	return statusResult{"written"}, nil
}

type pathsArgs struct {
	SandboxID        string   `json:"sandbox_id"`
	Paths            []string `json:"paths"`
	ConnectIfMissing bool     `json:"connect_if_missing"`
}

func (s *Sandboxes) fileDelete(ctx context.Context, call *Call) (any, error) {
	return s.withPaths(ctx, call, "deleted", func(ctx context.Context, ex *osbclient.Execd, p []string) error {
		return ex.DeleteFiles(ctx, p...)
	})
}

func (s *Sandboxes) rmdirs(ctx context.Context, call *Call) (any, error) {
	return s.withPaths(ctx, call, "deleted", func(ctx context.Context, ex *osbclient.Execd, p []string) error {
		return ex.DeleteDirs(ctx, p...)
	})
}

func (s *Sandboxes) withPaths(ctx context.Context, call *Call, done string, fn func(context.Context, *osbclient.Execd, []string) error) (any, error) {
	var a pathsArgs
	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	if len(a.Paths) == 0 {
		return nil, errors.New("paths is required and must name at least one path")
	}

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		return nil, err
	}

	if err := fn(ctx, ex, a.Paths); err != nil {
		return nil, s.execdErr(a.SandboxID, err)
	}

	return statusResult{done}, nil
}

type entryInfo struct {
	Path       string `json:"path"`
	Type       any    `json:"type"`
	Mode       int    `json:"mode"`
	Owner      string `json:"owner"`
	Group      string `json:"group"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
	CreatedAt  string `json:"created_at"`
}

func (s *Sandboxes) fileSearch(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID        string `json:"sandbox_id"`
		Path             string `json:"path"`
		Pattern          string `json:"pattern"`
		ConnectIfMissing bool   `json:"connect_if_missing"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		return nil, err
	}

	found, err := ex.SearchFiles(ctx, a.Path, a.Pattern)
	if err != nil {
		return nil, s.execdErr(a.SandboxID, err)
	}

	out := make([]entryInfo, 0, len(found))
	for _, f := range found {
		var typ any
		if f.Type != "" {
			typ = f.Type
		}

		out = append(out, entryInfo{
			Path: f.Path, Type: typ, Mode: f.Mode, Owner: f.Owner, Group: f.Group, Size: f.Size,
			ModifiedAt: timeStr(f.ModifiedAt), CreatedAt: timeStr(f.CreatedAt),
		})
	}

	return out, nil
}

func (s *Sandboxes) mkdirs(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID string `json:"sandbox_id"`
		Entries   []struct {
			Path  string  `json:"path"`
			Mode  *int    `json:"mode"`
			Owner *string `json:"owner"`
			Group *string `json:"group"`
		} `json:"entries"`
		ConnectIfMissing bool `json:"connect_if_missing"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	if len(a.Entries) == 0 {
		return nil, errors.New("entries is required and must name at least one directory")
	}

	dirs := map[string]osbclient.Permission{}

	for _, e := range a.Entries {
		mode := 755
		if e.Mode != nil {
			mode = *e.Mode
		}

		dirs[e.Path] = osbclient.Permission{Mode: mode, Owner: deref(e.Owner), Group: deref(e.Group)}
	}

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		return nil, err
	}

	if err := ex.MakeDirs(ctx, dirs); err != nil {
		return nil, s.execdErr(a.SandboxID, err)
	}

	return statusResult{"created"}, nil
}

// moveEntry accepts both the alias upstream's schema advertises (source/destination) and the
// field names its model also takes (src/dest), since the model populates by either.
type moveEntry struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Src         string `json:"src"`
	Dest        string `json:"dest"`
}

func (s *Sandboxes) move(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID        string      `json:"sandbox_id"`
		Entries          []moveEntry `json:"entries"`
		ConnectIfMissing bool        `json:"connect_if_missing"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	if len(a.Entries) == 0 {
		return nil, errors.New("entries is required and must name at least one move")
	}

	moves := make([]osbclient.Move, 0, len(a.Entries))

	for i, e := range a.Entries {
		m := osbclient.Move{Src: firstOf(e.Source, e.Src), Dest: firstOf(e.Destination, e.Dest)}
		if m.Src == "" || m.Dest == "" {
			return nil, fmt.Errorf("entries[%d] needs both source and destination", i)
		}

		moves = append(moves, m)
	}

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		return nil, err
	}

	if err := ex.MoveFiles(ctx, moves...); err != nil {
		return nil, s.execdErr(a.SandboxID, err)
	}

	return statusResult{"moved"}, nil
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

func (s *Sandboxes) replace(ctx context.Context, call *Call) (any, error) {
	var a struct {
		SandboxID string `json:"sandbox_id"`
		Entries   []struct {
			Path       string  `json:"path"`
			OldContent string  `json:"old_content"`
			NewContent *string `json:"new_content"`
		} `json:"entries"`
		ConnectIfMissing bool `json:"connect_if_missing"`
	}

	if err := call.Bind(&a); err != nil {
		return nil, err
	}

	if len(a.Entries) == 0 {
		return nil, errors.New("entries is required and must name at least one edit")
	}

	edits := map[string]osbclient.Replacement{}
	order := make([]string, 0, len(a.Entries))

	for i, e := range a.Entries {
		if e.Path == "" || e.OldContent == "" || e.NewContent == nil {
			return nil, fmt.Errorf("entries[%d] needs path, a non-empty old_content, and new_content", i)
		}

		// The contract keys edits by path, so two edits to one file in one call cannot both
		// be sent. Say so rather than silently keeping the last.
		if _, dup := edits[e.Path]; dup {
			return nil, fmt.Errorf("entries has two edits for %s; send them in separate calls", e.Path)
		}

		edits[e.Path] = osbclient.Replacement{Old: e.OldContent, New: *e.NewContent}
		order = append(order, e.Path)
	}

	ex, err := s.execdFor(ctx, a.SandboxID)
	if err != nil {
		return nil, err
	}

	counts, err := ex.ReplaceInFiles(ctx, edits)
	if err != nil {
		return nil, s.execdErr(a.SandboxID, err)
	}

	out := make([]map[string]any, 0, len(order))
	for _, p := range order {
		out = append(out, map[string]any{"path": p, "replaced_count": counts[p]})
	}

	return out, nil
}
