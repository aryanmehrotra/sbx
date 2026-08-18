package daemon

// Controlling a deployed sandbox over the connect endpoint.
//
// `sbx ui --connect` was read-only at first: the token bought reading what is fronted and
// carrying bytes, and nothing that changed state. The operator asked for the whole of the CLI
// instead - wake, sleep, re-limit, remove, logs - authorised by the same token, and this is
// that. It is a deliberate widening of what the token is worth: a leaked one now controls the
// deployment rather than only reading it, which is why every one of these still passes through
// the same `authed` gate as the fleet listing and the tunnel.
//
// None of it works in front mode. `--front` has no container runtime behind it - the platform
// owns that container - so there is nothing here to wake or sleep, and each handler says so
// rather than pretending. Control means something only where sbx was deployed with a provider:
// a VM with the docker socket mounted, or a cluster with deploy/activator.yaml.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// controlProvider is the provider these handlers act through, or an error shaped for the client
// when there is none. The error is a 409 rather than a 500: nothing failed, the endpoint simply
// does not manage anything, and the message points at the two shapes where it would.
func (d *daemon) controlProvider(verb string) (provider.Provider, int, string) {
	if d.provider == nil {
		return nil, http.StatusConflict, fmt.Sprintf(
			"%s is not available here: this endpoint only fronts a port, it does not manage a "+
				"container runtime. Deploy sbx with the docker socket mounted, or in a cluster "+
				"with deploy/activator.yaml, for control to mean something", verb)
	}

	return d.provider, 0, ""
}

// controlWake starts a service. Waking by connecting is the local dashboard's path, but a
// remote dashboard has no local listener to dial, so the wake is asked for explicitly.
func (d *daemon) controlWake(w http.ResponseWriter, r *http.Request) {
	d.actOnRef(w, r, "waking a service", func(p provider.Provider, ref string) error {
		return p.Start(r.Context(), ref)
	})
}

func (d *daemon) controlSleep(w http.ResponseWriter, r *http.Request) {
	d.actOnRef(w, r, "sleeping a service", func(p provider.Provider, ref string) error {
		return p.Stop(r.Context(), ref)
	})
}

// controlLimit sets a service's ceiling. cpu and mem are the raw provider units - nanoCPUs and
// bytes - because the wire is between two copies of sbx, and the parsing of "0.5"/"512m" into
// them already happened on the side that took it from the user.
func (d *daemon) controlLimit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ref      string `json:"ref"`
		NanoCPUs int64  `json:"nano_cpus"`
		MemBytes uint64 `json:"mem_bytes"`
	}

	if !decodeBody(w, r, &body) {
		return
	}

	p, status, msg := d.controlProvider("setting a limit")
	if p == nil {
		http.Error(w, msg, status)

		return
	}

	lim, ok := p.(provider.Limiter)
	if !ok {
		http.Error(w, "this backend cannot set limits", http.StatusConflict)

		return
	}

	if err := lim.SetLimits(r.Context(), body.Ref, provider.Limits{
		NanoCPUs: body.NanoCPUs, MemBytes: body.MemBytes,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)

		return
	}

	writeOK(w)
}

// controlRemove destroys a sandbox and its volume. It takes a sandbox name rather than a ref
// because that is what remove means - the whole group, not one service - and it is the one verb
// here that cannot be undone, which is worth remembering when the token that reaches it is the
// same one printed in a deploy's environment.
func (d *daemon) controlRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sandbox string `json:"sandbox"`
	}

	if !decodeBody(w, r, &body) {
		return
	}

	if body.Sandbox == "" {
		http.Error(w, "which sandbox: pass a sandbox name", http.StatusBadRequest)

		return
	}

	p, status, msg := d.controlProvider("removing a sandbox")
	if p == nil {
		http.Error(w, msg, status)

		return
	}

	if err := p.Remove(r.Context(), body.Sandbox); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)

		return
	}

	writeOK(w)
}

// controlLogs streams a service's output to the response body. Not following: a dashboard reads
// a tail and shows it, and a long-lived follow over a proxy that reaps idle connections is a
// different problem than the one this solves.
func (d *daemon) controlLogs(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		http.Error(w, "which service: pass ?ref=", http.StatusBadRequest)

		return
	}

	tail := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tail = n
		}
	}

	p, status, msg := d.controlProvider("reading logs")
	if p == nil {
		http.Error(w, msg, status)

		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	if err := p.Logs(r.Context(), ref, tail, false, w); err != nil {
		// The header is already sent, so this cannot become a status code. It goes in the body
		// where the client shows it beside whatever log did arrive.
		_, _ = fmt.Fprintf(w, "\n[sbx: reading logs failed: %v]\n", err)
	}
}

// actOnRef is the shared shape of wake and sleep: pull a ref from the body, refuse in front
// mode, act, and report.
func (d *daemon) actOnRef(w http.ResponseWriter, r *http.Request, verb string,
	do func(provider.Provider, string) error) {
	var body struct {
		Ref string `json:"ref"`
	}

	if !decodeBody(w, r, &body) {
		return
	}

	if body.Ref == "" {
		http.Error(w, "which service: pass a ref", http.StatusBadRequest)

		return
	}

	p, status, msg := d.controlProvider(verb)
	if p == nil {
		http.Error(w, msg, status)

		return
	}

	if err := do(p, body.Ref); err != nil {
		// BadGateway: the endpoint is fine, the thing behind it refused. A client telling those
		// apart is a client that knows whether to retry here or go look at the deployment.
		http.Error(w, err.Error(), http.StatusBadGateway)

		return
	}

	writeOK(w)
}

// decodeBody reads a small JSON body, bounded like the fleet reply is. It writes the error
// response itself and returns false when the caller should stop.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFleetBody)).Decode(v); err != nil {
		http.Error(w, "could not read the request body: "+err.Error(), http.StatusBadRequest)

		return false
	}

	return true
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}
