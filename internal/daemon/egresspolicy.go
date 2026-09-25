package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Changing a sandbox's egress policy while it runs.
//
// The policy lives with the filter that enforces it, because that is the only copy that
// matters: a filter in a container holds it in memory and on its own disk, and a filter that is
// a listener inside the daemon holds it in memory. What this file adds is one Go API over both,
// and a copy on the host that outlives either - so a daemon restart, or a filter container
// that was replaced, comes back enforcing what it was last told rather than what the spec said
// at create. DECISIONS.md, "A live egress policy is held by the filter and pushed to it", has
// why the control channel is shaped this way.

// ErrSharedFilter is a write naming one service of several that share a filter. A policy is a
// property of the sandbox's filter, not of one container behind it.
var ErrSharedFilter = errors.New("services share one egress filter")

// errConflict is a write that lost a race with another writer; the caller re-reads and retries.
var errConflict = errors.New("the policy changed while it was being updated")

// EgressControl reads and changes the egress policy of running sandboxes. It is safe for
// concurrent use. The CLI builds one per command; a running daemon owns one whose writes reach
// the filters it hosts immediately (see (*daemon).Egress).
type EgressControl struct {
	provider provider.Provider
	dir      string
	client   *http.Client

	// apply swaps the policy of a filter the calling process hosts, keyed by gateway, and
	// reports whether it found one. Nil outside a daemon.
	apply func(gateway string, p egress.Policy) bool

	mu sync.Mutex
}

// NewEgressControl returns the API over p's sandboxes. dir is where live policies are kept on the
// host; "" is ~/.sbx/egress, beside the daemon's presence file.
func NewEgressControl(p provider.Provider, dir string) *EgressControl {
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".sbx", "egress")
		}
	}

	return &EgressControl{provider: p, dir: dir, client: &http.Client{Timeout: 5 * time.Second}}
}

// EgressHTTPStatus maps an error from EgressControl to the status the OpenSandbox API answers
// it with: a refused policy is 400, an unknown sandbox 404, a sandbox with no filter to change -
// or a write that names one of several services sharing one - 409, and anything else 500.
func EgressHTTPStatus(err error) int {
	var bad *egress.Error

	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &bad):
		return http.StatusBadRequest
	case errors.Is(err, provider.ErrNoSandbox):
		return http.StatusNotFound
	case errors.Is(err, provider.ErrNotFiltered), errors.Is(err, ErrSharedFilter):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// GetPolicy returns the policy in force for sandbox. service may be "" or any of its filtered
// services; they all answer with the one policy their shared filter enforces.
func (c *EgressControl) GetPolicy(ctx context.Context, sandbox, service string) (egress.Status, error) {
	f, err := c.locate(ctx, sandbox, service, false)
	if err != nil {
		return egress.Status{}, err
	}

	p, _, err := c.current(ctx, f)
	if err != nil {
		return egress.Status{}, err
	}

	return egress.StatusOf(p, c.note(f)), nil
}

// SetPolicy replaces the policy: default action and every rule (OpenSandbox PUT).
func (c *EgressControl) SetPolicy(ctx context.Context, sandbox, service string, p egress.Policy) (egress.Status, error) {
	n, err := p.Normalize()
	if err != nil {
		return egress.Status{}, err
	}

	return c.mutate(ctx, sandbox, service, func(egress.Policy) (egress.Policy, error) { return n, nil })
}

// PatchPolicy merges rules into the policy with OpenSandbox's PATCH semantics (egress.Policy.Merge).
func (c *EgressControl) PatchPolicy(ctx context.Context, sandbox, service string, rules []egress.Rule) (egress.Status, error) {
	if len(rules) == 0 {
		return egress.Status{}, &egress.Error{Code: egress.CodeInvalidRequest,
			Message: "invalid patch rules: empty array - send at least one rule"}
	}

	patch, err := (egress.Policy{Egress: rules}).Normalize()
	if err != nil {
		return egress.Status{}, err
	}

	return c.mutate(ctx, sandbox, service, func(cur egress.Policy) (egress.Policy, error) {
		return cur.Merge(patch.Egress), nil
	})
}

// DeleteRules removes the rules for targets, keeping the default (OpenSandbox DELETE). A target
// with no rule is ignored.
func (c *EgressControl) DeleteRules(ctx context.Context, sandbox, service string, targets []string) (egress.Status, error) {
	if len(targets) == 0 {
		return egress.Status{}, &egress.Error{Code: egress.CodeInvalidRequest,
			Message: "invalid delete targets: empty array - name at least one target"}
	}

	return c.mutate(ctx, sandbox, service, func(cur egress.Policy) (egress.Policy, error) {
		p, _ := cur.Remove(targets)
		return p, nil
	})
}

// SetDefault changes only the default action, keeping every rule.
func (c *EgressControl) SetDefault(ctx context.Context, sandbox, service, action string) (egress.Status, error) {
	return c.mutate(ctx, sandbox, service, func(cur egress.Policy) (egress.Policy, error) {
		cur.DefaultAction = action
		return cur.Normalize()
	})
}

// ResetPolicy returns the sandbox to the policy its spec declared and forgets the live one.
func (c *EgressControl) ResetPolicy(ctx context.Context, sandbox, service string) (egress.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	f, err := c.locate(ctx, sandbox, service, true)
	if err != nil {
		return egress.Status{}, err
	}

	if err := c.push(ctx, f, f.Declared, ""); err != nil {
		return egress.Status{}, err
	}

	if err := c.forget(sandbox); err != nil {
		return egress.Status{}, fmt.Errorf("the declared policy is in force, but the saved live "+
			"one could not be removed and would come back after a restart: %w", err)
	}

	return egress.StatusOf(f.Declared, c.note(f)), nil
}

// Forget drops the saved live policy of a sandbox that has been removed, so a new sandbox that
// reuses the name starts from its own spec rather than inheriting a stranger's exceptions.
func (c *EgressControl) Forget(sandbox string) error { return c.forget(sandbox) }

// Sync pushes a sandbox's saved live policy to its container filter if the filter is enforcing
// something else - a container that was replaced, or one that restarted from an older copy. A
// sandbox with no saved policy is left alone: its filter is already running what it was created
// with.
func (c *EgressControl) Sync(ctx context.Context, sandbox string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	saved, ok := c.load(sandbox)
	if !ok {
		return nil
	}

	f, err := c.locate(ctx, sandbox, "", false)
	if err != nil {
		return err
	}

	if f.Control == "" {
		return nil // hosted by the daemon, which reads the saved copy itself
	}

	if saved.Declared != f.Declared.Hash() {
		// Made against a different declaration - the spec changed and the sandbox was
		// recreated. The live change was an exception to a policy that no longer exists.
		return c.forget(sandbox)
	}

	cur, etag, err := c.current(ctx, f)
	if err != nil {
		return err
	}

	if cur.Hash() == saved.Policy.Hash() {
		return nil
	}

	return c.push(ctx, f, saved.Policy, etag)
}

// mutate is every write: read what is in force, compute the new policy, write it back - retried
// when another writer got in between, which If-Match detects on a container filter.
func (c *EgressControl) mutate(ctx context.Context, sandbox, service string,
	next func(egress.Policy) (egress.Policy, error),
) (egress.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	f, err := c.locate(ctx, sandbox, service, true)
	if err != nil {
		return egress.Status{}, err
	}

	for range 3 {
		cur, etag, err := c.current(ctx, f)
		if err != nil {
			return egress.Status{}, err
		}

		p, err := next(cur)
		if err != nil {
			return egress.Status{}, err
		}

		err = c.push(ctx, f, p, etag)
		if errors.Is(err, errConflict) {
			continue
		}

		if err != nil {
			return egress.Status{}, err
		}

		if err := c.save(f, p); err != nil {
			return egress.Status{}, fmt.Errorf("the policy is in force but could not be saved, so "+
				"a restart of the daemon or the filter would revert it: %w", err)
		}

		return egress.StatusOf(p, c.note(f)), nil
	}

	return egress.Status{}, fmt.Errorf("the policy of %q kept changing underneath this update; "+
		"try again", sandbox)
}

func (c *EgressControl) locate(ctx context.Context, sandbox, service string, write bool) (provider.EgressFilter, error) {
	ef, ok := c.provider.(provider.EgressFilters)
	if !ok {
		return provider.EgressFilter{}, fmt.Errorf("%w: the %s provider has no egress filter "+
			"to change", provider.ErrNotFiltered, c.provider.Name())
	}

	f, err := ef.EgressFilter(ctx, sandbox)
	if err != nil {
		return provider.EgressFilter{}, err
	}

	switch {
	case service == "":
	case !slices.Contains(f.Services, service):
		return provider.EgressFilter{}, fmt.Errorf("%w: service %q of %q is not behind the egress "+
			"filter (the filtered services are %s)", provider.ErrNotFiltered, service, sandbox,
			strings.Join(f.Services, ", "))
	case write && len(f.Services) > 1:
		return provider.EgressFilter{}, fmt.Errorf("%w: %s of %q all go through one filter, so a "+
			"policy cannot be changed for %q alone. Leave the service out to change it for all "+
			"of them, or give %q a sandbox of its own", ErrSharedFilter,
			strings.Join(f.Services, ", "), sandbox, service, service)
	}

	return f, nil
}

// current is the policy in force and a tag identifying it for If-Match.
func (c *EgressControl) current(ctx context.Context, f provider.EgressFilter) (egress.Policy, string, error) {
	if f.Control == "" {
		if saved, ok := c.load(f.Sandbox); ok && saved.Declared == f.Declared.Hash() {
			return saved.Policy, "", nil
		}

		return f.Declared, "", nil
	}

	resp, err := c.call(ctx, f, http.MethodGet, nil, "")
	if err != nil {
		return egress.Policy{}, "", err
	}

	var st egress.Status
	if err := json.Unmarshal(resp, &st); err != nil || st.Policy == nil {
		return egress.Policy{}, "", fmt.Errorf("the egress filter of %q answered with something "+
			"that is not a policy: %.200q", f.Sandbox, resp)
	}

	return *st.Policy, st.Policy.Hash(), nil
}

// push puts p in force on the filter.
func (c *EgressControl) push(ctx context.Context, f provider.EgressFilter, p egress.Policy, etag string) error {
	if f.Control != "" {
		body, err := json.Marshal(p)
		if err != nil {
			return err
		}

		_, err = c.call(ctx, f, http.MethodPut, body, etag)

		return err
	}

	// A filter that is the daemon's own listener. In the daemon this swaps it now; from the
	// CLI the saved copy is what reaches it, within a second (watchEgress).
	if c.apply != nil {
		c.apply(f.Gateway, p)
	}

	return nil
}

func (c *EgressControl) call(ctx context.Context, f provider.EgressFilter, method string, body []byte, etag string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+f.Control+"/policy", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set(egress.TokenHeader, f.Token)

	if etag != "" {
		req.Header.Set("If-Match", etag)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the egress filter of %q did not answer on %s (is its container "+
			"running? docker start sbx-egressfilter-%s): %w", f.Sandbox, f.Control, f.Sandbox, err)
	}

	defer resp.Body.Close()

	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusConflict:
		return nil, errConflict
	case resp.StatusCode == http.StatusBadRequest:
		return nil, &egress.Error{Code: egress.CodeInvalidRequest, Message: strings.TrimSpace(string(out))}
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the egress filter of %q refused the %s: %s %s",
			f.Sandbox, method, resp.Status, strings.TrimSpace(string(out)))
	}

	return out, nil
}

// note is the reason reported beside a policy that is only saved, not yet in force - a filter
// hosted by a daemon this process is not.
func (c *EgressControl) note(f provider.EgressFilter) string {
	if f.Control != "" || c.apply != nil {
		return ""
	}

	if _, running := Running(); !running {
		return "saved; this sandbox's filter runs inside sbx serve, which is not running, so " +
			"nothing leaves the sandbox until it starts - and then under this policy"
	}

	return "saved; sbx serve hosts this sandbox's filter and applies it within a second"
}

// saved is the host's copy of a live policy.
type saved struct {
	Sandbox string        `json:"sandbox"`
	Gateway string        `json:"gateway"`
	Policy  egress.Policy `json:"policy"`

	// Declared is the hash of the spec's policy this one was made against. A sandbox recreated
	// from a different spec does not inherit an exception to a policy it no longer has.
	Declared string `json:"declared"`
}

func (c *EgressControl) path(sandbox string) (string, error) {
	if c.dir == "" {
		return "", errors.New("no home directory to keep live egress policies in")
	}

	if sandbox == "" || filepath.Base(sandbox) != sandbox || strings.HasPrefix(sandbox, ".") {
		return "", fmt.Errorf("%q is not a sandbox name", sandbox)
	}

	return filepath.Join(c.dir, sandbox+".json"), nil
}

func (c *EgressControl) load(sandbox string) (saved, bool) {
	p, err := c.path(sandbox)
	if err != nil {
		return saved{}, false
	}

	b, err := os.ReadFile(p)
	if err != nil {
		return saved{}, false
	}

	var s saved
	if err := json.Unmarshal(b, &s); err != nil {
		return saved{}, false
	}

	n, err := s.Policy.Normalize()
	if err != nil {
		return saved{}, false
	}

	s.Policy = n

	return s, true
}

func (c *EgressControl) save(f provider.EgressFilter, p egress.Policy) error {
	path, err := c.path(f.Sandbox)
	if err != nil {
		return err
	}

	b, err := json.MarshalIndent(saved{Sandbox: f.Sandbox, Gateway: f.Gateway, Policy: p,
		Declared: f.Declared.Hash()}, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	// Renamed into place, so the daemon's watcher never reads half a file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

func (c *EgressControl) forget(sandbox string) error {
	p, err := c.path(sandbox)
	if err != nil {
		return err
	}

	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

// Handler serves one sandbox's policy with the OpenSandbox egress sidecar's /policy semantics
// (specs/egress-api.yaml): GET reads; POST and PUT replace, an empty body resetting to deny-all
// as upstream's handlePost does; PATCH merges an array of rules; DELETE removes an array of
// targets. Errors are text/plain, as that spec's responses are. It is what an OpenSandbox SDK
// reaches through the endpoint of port 18080, where upstream runs its sidecar.
func (c *EgressControl) Handler(sandbox string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			st  egress.Status
			err error
		)

		body, rerr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if rerr != nil {
			http.Error(w, "failed to read body: "+rerr.Error(), http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodGet:
			st, err = c.GetPolicy(r.Context(), sandbox, "")
		case http.MethodPost, http.MethodPut:
			var p egress.Policy
			if p, err = egress.ParsePolicy(body); err == nil {
				st, err = c.SetPolicy(r.Context(), sandbox, "", p)
			}
		case http.MethodPatch:
			var rules []egress.Rule
			if rules, err = egress.ParseRules(body); err == nil {
				st, err = c.PatchPolicy(r.Context(), sandbox, "", rules)
			}
		case http.MethodDelete:
			var targets []string
			if targets, err = egress.ParseTargets(body); err == nil {
				st, err = c.DeleteRules(r.Context(), sandbox, "", targets)
			}
		default:
			w.Header().Set("Allow", "GET, POST, PUT, PATCH, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		if err != nil {
			http.Error(w, err.Error(), EgressHTTPStatus(err))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
}
