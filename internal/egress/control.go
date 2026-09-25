package egress

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// The filter's own control API: read and replace the policy of a filter that is running.
//
// It exists for the filter that runs as a container. The daemon cannot reach into that
// container's memory the way it swaps the policy of a filter it hosts itself, so the container
// answers on the loopback port it already publishes for activity scraping, and the daemon asks
// it. Only replace is offered: merge, remove and reset are computed by the caller from what GET
// returned, and If-Match makes that read-modify-write safe against a second writer.
//
// This file is compiled into the filter container verbatim (see BuildContext).

// TokenHeader carries the control token. The endpoint is reachable from the sandbox's own
// bridge - the filter container listens on every interface it has, and the workload is on one
// of them - so without a token the workload could simply rewrite its own policy.
const TokenHeader = "X-Sbx-Egress-Token"

// Control serves GET and PUT /policy for one Filter.
type Control struct {
	Filter *Filter

	// Token must be presented on every request. Empty refuses everything: a control endpoint
	// with no secret is not an option, it is the hole described above.
	Token string

	// Persist, when set, records a policy before it is put in force, so a restart comes back
	// enforcing what it was told last rather than what it was started with.
	Persist func(Policy) error

	mu sync.Mutex
}

func (c *Control) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get(TokenHeader)), []byte(c.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if r.URL.Path != "/policy" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		c.reply(w, c.Filter.Policy())
	case http.MethodPut:
		c.put(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (c *Control) put(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	p, err := ParsePolicy(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if want := r.Header.Get("If-Match"); want != "" && want != c.Filter.Policy().Hash() {
		http.Error(w, "the policy changed since it was read; read it again and retry", http.StatusConflict)
		return
	}

	if c.Persist != nil {
		if err := c.Persist(p); err != nil {
			http.Error(w, "the policy could not be saved, so it was not applied: "+err.Error(),
				http.StatusInternalServerError)

			return
		}
	}

	if err := c.Filter.SetPolicy(p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	c.reply(w, p)
}

func (c *Control) reply(w http.ResponseWriter, p Policy) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", p.Hash())
	_ = json.NewEncoder(w).Encode(StatusOf(p, ""))
}
