package egress

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sync"
)

// The filter's own control API: read and replace the policy of a filter that is running, push it
// the addresses it must refuse, and read when it last carried a byte.
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

// RefuseSet is the body of PUT /refuse and the answer to GET /refuse: addresses and CIDRs, as
// ParsePrefixes reads them one at a time.
type RefuseSet struct {
	Prefixes []string `json:"prefixes"`
}

// Control serves, behind the token:
//
//	GET, PUT /policy   the policy in force
//	GET, PUT /refuse   the addresses the daemon says are the machine behind the filter (Doors)
//	GET /last          when the filter last carried a permitted byte, in Unix nanoseconds
//
// Every path is behind the token, /last included. The workload can reach this port, and /last
// answered it once: it is the daemon's idle signal, and a sandbox has no business reading
// another's view of its own activity - nor a way to learn it is being watched for sleep.
type Control struct {
	Filter *Filter

	// Token must be presented on every request. Empty refuses everything: a control endpoint
	// with no secret is not an option, it is the hole described above.
	Token string

	// Persist, when set, records a policy before it is put in force, so a restart comes back
	// enforcing what it was told last rather than what it was started with.
	Persist func(Policy) error

	// Doors is the filter's refused set, or nil to serve no /refuse.
	Doors *Doors

	// PersistRefuse, when set, records a pushed set after it is put in force. After, not before
	// as Persist is: a refusal applied and not saved is the safe half to have done.
	PersistRefuse func([]netip.Prefix) error

	// Last reports the last activity, or is nil to serve no /last.
	Last func() int64

	mu sync.Mutex
}

func (c *Control) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get(TokenHeader)), []byte(c.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch {
	case r.URL.Path == "/policy":
	case r.URL.Path == "/refuse" && c.Doors != nil:
		c.refuse(w, r)
		return
	case r.URL.Path == "/last" && c.Last != nil && r.Method == http.MethodGet:
		fmt.Fprintf(w, "%d\n", c.Last())
		return
	default:
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

// refuse reads or replaces the pushed half of the refused set. A body with any entry that does not
// parse changes nothing: the set in force stays, which is the closed side.
func (c *Control) refuse(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		var in RefuseSet
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, "want {\"prefixes\":[...]}: "+err.Error(), http.StatusBadRequest)
			return
		}

		var set []netip.Prefix

		for _, s := range in.Prefixes {
			p, err := ParsePrefixes(s)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			set = append(set, p...)
		}

		c.mu.Lock()
		c.Doors.SetPushed(set)

		var err error
		if c.PersistRefuse != nil {
			err = c.PersistRefuse(set)
		}
		c.mu.Unlock()

		if err != nil {
			http.Error(w, "the set is in force but could not be saved, so a restart would drop it "+
				"until the next push: "+err.Error(), http.StatusInternalServerError)

			return
		}
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	out := RefuseSet{Prefixes: []string{}}
	for _, p := range c.Doors.Prefixes() {
		out.Prefixes = append(out.Prefixes, p.String())
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (c *Control) reply(w http.ResponseWriter, p Policy) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", p.Hash())
	_ = json.NewEncoder(w).Encode(StatusOf(p, ""))
}
