package jupyter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// rest is the slice of the Jupyter Server REST API the engine uses.
type rest struct {
	base   *url.URL
	token  string
	client *http.Client
}

// statusError is a non-2xx answer from Jupyter.
type statusError struct {
	method, path string
	status       int
	body         string
}

func (e *statusError) Error() string {
	msg := fmt.Sprintf("jupyter %s %s answered %d", e.method, e.path, e.status)

	switch e.status {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg += fmt.Sprintf(" - the token was refused; check %s matches the server's token", EnvToken)
	}

	if e.body != "" {
		msg += ": " + e.body
	}

	return msg
}

// transient reports whether an error is the server not being up yet, as opposed to the server
// being up and saying no. Only the first kind is worth retrying.
func transient(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.status >= 500 || se.status == http.StatusTooManyRequests
	}

	// Anything that got no HTTP answer at all - refused, reset, timed out - is a server that is
	// not listening yet. The caller's context still bounds the retrying.
	var ue *url.Error

	return errors.As(err, &ue)
}

func (r *rest) url(path string) string {
	u := *r.base
	u.Path = r.base.Path + path

	return u.String()
}

func (r *rest) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader

	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}

		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, r.url(path), body)
	if err != nil {
		return err
	}

	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if r.token != "" {
		req.Header.Set("Authorization", "token "+r.token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("jupyter %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

		return &statusError{method: method, path: path, status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)

		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("jupyter %s %s: decode response: %w", method, path, err)
	}

	return nil
}

type kernelSpecs struct {
	Default     string `json:"default"`
	Kernelspecs map[string]struct {
		Name string `json:"name"`
		Spec struct {
			Language    string `json:"language"`
			DisplayName string `json:"display_name"`
		} `json:"spec"`
	} `json:"kernelspecs"`
}

func (r *rest) kernelSpecs(ctx context.Context) (*kernelSpecs, error) {
	var ks kernelSpecs
	if err := r.do(ctx, http.MethodGet, "/api/kernelspecs", nil, &ks); err != nil {
		return nil, err
	}

	return &ks, nil
}

// pickKernel chooses the kernelspec for a language.
//
// Upstream skips the spec named "python3" whenever it searches: the code-interpreter image
// installs its own versioned Python kernel next to ipykernel's default, and the default is the
// wrong interpreter. The skip is kept, but as a preference - a plain Jupyter image has only
// "python3", and refusing Python there would be a regression nobody asked for. Among several
// candidates the choice is by sorted name, so it is the same on every call; upstream's is map
// order, i.e. random.
func (ks *kernelSpecs) pickKernel(lang string) (string, error) {
	names := make([]string, 0, len(ks.Kernelspecs))
	for n := range ks.Kernelspecs {
		names = append(names, n)
	}

	sort.Strings(names)

	var fallback string

	langs := map[string]bool{}

	for _, n := range names {
		l := normalizeLanguage(ks.Kernelspecs[n].Spec.Language)
		langs[l] = true

		if l != lang {
			continue
		}

		if n == "python3" {
			fallback = n

			continue
		}

		return n, nil
	}

	if fallback != "" {
		return fallback, nil
	}

	have := make([]string, 0, len(langs))
	for l := range langs {
		have = append(have, l)
	}

	sort.Strings(have)

	return "", fmt.Errorf("%w: no Jupyter kernel for %q is installed (kernels here are for: %s) - install one in the image or use another language",
		ErrUnsupportedLanguage, lang, strings.Join(have, ", "))
}

type sessionResp struct {
	ID     string `json:"id"`
	Kernel struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"kernel"`
}

// createSession starts a kernel through the sessions API, as upstream does. A session rather
// than a bare kernel because the session carries the notebook path, and the path's directory is
// where Jupyter starts the kernel - which is how a context gets its working directory.
func (r *rest) createSession(ctx context.Context, name, path, kernel string) (*sessionResp, error) {
	in := map[string]any{
		"path":   path,
		"name":   name,
		"type":   "notebook",
		"kernel": map[string]string{"name": kernel},
	}

	var s sessionResp
	if err := r.do(ctx, http.MethodPost, "/api/sessions", in, &s); err != nil {
		return nil, err
	}

	if s.ID == "" || s.Kernel.ID == "" {
		return nil, fmt.Errorf("jupyter POST /api/sessions returned no session or kernel id")
	}

	return &s, nil
}

// deleteSession removes a session and shuts its kernel down. A 404 is success: the goal is that
// it does not exist.
func (r *rest) deleteSession(ctx context.Context, id string) error {
	err := r.do(ctx, http.MethodDelete, "/api/sessions/"+url.PathEscape(id), nil, nil)

	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusNotFound {
		return nil
	}

	return err
}

func (r *rest) interrupt(ctx context.Context, kernelID string) error {
	return r.do(ctx, http.MethodPost, "/api/kernels/"+url.PathEscape(kernelID)+"/interrupt", nil, nil)
}

// channelsURL is the kernel's websocket endpoint, with the base path kept - a Jupyter behind a
// base_url is otherwise unreachable.
func (r *rest) channelsURL(kernelID, sessionID string) string {
	u := *r.base
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}

	u.Path = r.base.Path + "/api/kernels/" + url.PathEscape(kernelID) + "/channels"
	u.RawQuery = url.Values{"session_id": {sessionID}}.Encode()

	return u.String()
}
