// Package jupyter is the code-interpreter engine behind execd's /code routes: it runs code in
// Jupyter kernels through a Jupyter Server that the sandbox image provides, and turns what the
// kernel says into execd's server-sent events.
//
// Nothing here starts Jupyter. The image does (opensandbox/code-interpreter runs it on 44771 and
// sets JUPYTER_HOST and JUPYTER_TOKEN), and an image without one gets a clear refusal from
// Available rather than a hang. Upstream's own execd is the behavioural reference: its context
// ids are Jupyter session ids, one execution runs per context at a time and a second is refused,
// and its event sequence - including its quirks - is reproduced so that clients written against
// it see the same stream.
package jupyter

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Environment variables, named as upstream names them so an image built for OpenSandbox needs no
// change.
const (
	// EnvHost is the Jupyter Server base URL, e.g. http://127.0.0.1:44771.
	EnvHost = "JUPYTER_HOST"
	// EnvToken is the Jupyter Server token.
	EnvToken = "JUPYTER_TOKEN"
	// EnvPort is read only when EnvHost is unset: the code-interpreter image exports it, and its
	// SDKs probe 127.0.0.1:${JUPYTER_PORT} for readiness, so it is a fair second guess.
	EnvPort = "JUPYTER_PORT"
)

// Config is how the engine reaches Jupyter.
type Config struct {
	// BaseURL is the Jupyter Server root, http:// or https://, optionally with a base path.
	BaseURL string
	// Token is sent as "Authorization: token <Token>" on REST calls and the kernel websocket.
	Token string
	// HTTPClient is used for REST calls. Nil means a client with a 30s timeout: starting a kernel
	// is the slowest call and takes seconds, not minutes.
	HTTPClient *http.Client
	// StartupWait bounds how long creating a context retries while the Jupyter server is not yet
	// answering - the first request can arrive while the image's entrypoint is still starting it.
	// Zero means 30s.
	StartupWait time.Duration
	// PingInterval is how often a running execution emits a ping event, which keeps idle proxies
	// from cutting a long, quiet cell. Zero means 3s, upstream's interval.
	PingInterval time.Duration
	// ReplyGrace bounds the wait for execute_reply after the kernel has gone idle. Zero means 5s.
	ReplyGrace time.Duration
}

// ConfigFromEnv reads the upstream variable names. An unset JUPYTER_HOST with JUPYTER_PORT set
// means 127.0.0.1 on that port; with neither, BaseURL is empty and Available says so.
func ConfigFromEnv() Config {
	cfg := Config{
		BaseURL: strings.TrimSpace(os.Getenv(EnvHost)),
		Token:   os.Getenv(EnvToken),
	}

	if cfg.BaseURL == "" {
		if port := strings.TrimSpace(os.Getenv(EnvPort)); port != "" {
			cfg.BaseURL = "http://127.0.0.1:" + port
		}
	}

	return cfg
}

// Sentinel errors. execd maps these to status codes; everything else is a 500.
var (
	// ErrNotConfigured means no Jupyter server is configured at all: the image has none.
	ErrNotConfigured = errors.New("no Jupyter server configured")
	// ErrUnavailable wraps a failure to reach a configured Jupyter server.
	ErrUnavailable = errors.New("jupyter server unavailable")
	// ErrContextNotFound is an unknown context id.
	ErrContextNotFound = errors.New("context not found")
	// ErrContextBusy means the context is already running code. Upstream refuses rather than
	// queues: a queued cell would run against state the caller has not seen yet.
	ErrContextBusy = errors.New("session is busy")
	// ErrUnsupportedLanguage is a language that is not run through Jupyter.
	ErrUnsupportedLanguage = errors.New("unsupported language")
)

// Languages lists the languages upstream runs through a Jupyter kernel. Others ("command",
// "sql", "background-command", or none) are execd's own and never reach this package.
var Languages = []string{"python", "bash", "java", "javascript", "typescript", "go"}

// IsKernelLanguage reports whether execd should route a language here.
func IsKernelLanguage(lang string) bool {
	lang = normalizeLanguage(lang)
	for _, l := range Languages {
		if l == lang {
			return true
		}
	}

	return false
}

func normalizeLanguage(lang string) string { return strings.ToLower(strings.TrimSpace(lang)) }

func (c Config) validate() (*url.URL, error) {
	if c.BaseURL == "" {
		return nil, fmt.Errorf("%w: %s is unset - code contexts need an image that runs Jupyter Server "+
			"(opensandbox/code-interpreter sets %s=http://127.0.0.1:44771 and %s)", ErrNotConfigured, EnvHost, EnvHost, EnvToken)
	}

	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: %s=%q must be an http:// or https:// URL with a host", ErrNotConfigured, EnvHost, c.BaseURL)
	}

	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""

	return u, nil
}

func (c Config) withDefaults() Config {
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}

	if c.StartupWait <= 0 {
		c.StartupWait = 30 * time.Second
	}

	if c.PingInterval <= 0 {
		c.PingInterval = 3 * time.Second
	}

	if c.ReplyGrace <= 0 {
		c.ReplyGrace = 5 * time.Second
	}

	return c
}
