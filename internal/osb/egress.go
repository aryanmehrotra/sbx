package osb

// A sandbox's egress policy, through both doors OpenSandbox has for it.
//
// The lifecycle API's /networkpolicy is the control-plane door. The other is the egress sidecar
// upstream runs inside the sandbox on port 18080, which the SDK reaches through
// endpoints/18080 + /policy. sbx has no sidecar in the sandbox - the filter is the daemon's, or a
// container beside the sandbox - so endpoints/18080 points back at this listener, and both
// doors lead to the same EgressControl. Neither holds a copy of the policy: the filter does.

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/egress"
)

const (
	egressPort = 18080

	// egressAuthHeader is what the SDK's egress client authenticates with
	// (sdks/sandbox/go/egress.go at release-1.1.0).
	egressAuthHeader = "OPENSANDBOX-EGRESS-AUTH"
)

var sidecarPath = regexp.MustCompile(`^/v1/sandboxes/[^/]+/egress/policy$`)

func isSidecarPath(p string) bool { return sidecarPath.MatchString(p) }

// createPolicy turns a create request's networkPolicy into the policy the sandbox starts with.
//
// Absent or null is no filter at all, which is what a caller who did not ask gets. An empty
// object is allow-all - the create spec: "If omitted or empty, the sidecar starts in allow-all
// mode until updated" - but still behind a filter, so it can be tightened later. Anything else is
// the sidecar's own body, parsed by egress with its defaults and refusals.
func createPolicy(raw []byte) (*egress.Policy, error) {
	t := strings.TrimSpace(string(raw))

	switch t {
	case "", "null":
		return nil, nil
	case "{}":
		p, err := egress.Policy{DefaultAction: egress.ActionAllow}.Normalize()
		return &p, err
	}

	p, err := egress.ParsePolicy(raw)
	if err != nil {
		return nil, err
	}

	return &p, nil
}

// networkPolicy serves GET/PUT/PATCH/DELETE /v1/sandboxes/{id}/networkpolicy.
func (s *Server) networkPolicy(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}

	if s.egress == nil {
		notYet(w, "network policies", "a daemon with egress control")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "SANDBOX::INVALID_PARAMETER", "reading the body: "+err.Error())
		return
	}

	var st egress.Status

	switch r.Method {
	case http.MethodGet:
		st, err = s.egress.GetPolicy(r.Context(), rec.ID, "")
	case http.MethodPut:
		var p egress.Policy
		if p, err = egress.ParsePolicy(body); err == nil {
			st, err = s.egress.SetPolicy(r.Context(), rec.ID, "", p)
		}
	case http.MethodPatch:
		var rules []egress.Rule
		if rules, err = egress.ParseRules(body); err == nil {
			st, err = s.egress.PatchPolicy(r.Context(), rec.ID, "", rules)
		}
	case http.MethodDelete:
		var targets []string
		if targets, err = egress.ParseTargets(body); err == nil {
			st, err = s.egress.DeleteRules(r.Context(), rec.ID, "", targets)
		}
	}

	if err != nil {
		s.egressErr(w, rec.ID, err)
		return
	}

	writeJSON(w, http.StatusOK, st)
}

// egressErr answers an egress failure in the lifecycle API's {code, message} shape, keeping
// egress's own code and message where it has one.
func (s *Server) egressErr(w http.ResponseWriter, id string, err error) {
	status := s.egressStatus(err)

	var bad *egress.Error
	if errors.As(err, &bad) {
		writeErr(w, http.StatusBadRequest, "SANDBOX::"+bad.Code, bad.Message)
		return
	}

	switch status {
	case http.StatusNotFound:
		writeErr(w, status, "SANDBOX::NOT_FOUND", fmt.Sprintf("%s: %v", id, err))
	case http.StatusConflict:
		writeErr(w, status, "SANDBOX::EGRESS_NOT_ENABLED", fmt.Sprintf("%s has no egress filter to "+
			"change (%v): it was created without a networkPolicy. Create it with one - even {} "+
			"for allow-all - to be able to change its policy while it runs", id, err))
	default:
		writeErr(w, status, "SANDBOX::EGRESS_FAILED", err.Error())
	}
}

// sidecarPolicy is the sandbox's egress sidecar /policy, served from here. It authenticates
// with the per-sandbox egress credential handed out in endpoints/18080's headers - never execd's
// token, which the workload holds - or, with no such header, the API key check in authed already
// passed, because the operator can do the same through /networkpolicy.
func (s *Server) sidecarPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	rec, ok := s.snapshot(id)

	if got := r.Header.Get(egressAuthHeader); got != "" {
		// One answer for "no such sandbox" and "wrong credential": this path skipped the API key,
		// so a 404 here would let anybody enumerate sandbox ids.
		if !ok || rec.EgressToken == "" ||
			subtle.ConstantTimeCompare([]byte(got), []byte(rec.EgressToken)) != 1 {
			http.Error(w, "invalid "+egressAuthHeader+": fetch it from GET /v1/sandboxes/{id}/endpoints/18080 with the API key", http.StatusUnauthorized)
			return
		}
	}

	if !validID(id) || !ok {
		http.Error(w, "no such sandbox", http.StatusNotFound)
		return
	}

	if s.egress == nil {
		http.Error(w, "this sbx serve has no egress control", http.StatusNotImplemented)
		return
	}

	s.egress.Handler(rec.ID).ServeHTTP(w, r)
}

// egressEndpoint is endpoints/18080: this listener, as the caller reached it, with the sandbox's
// egress credential. Minted on first ask - which also covers a record written before the
// credential existed - and stored only in the record, which is 0600 and never in the container.
func (s *Server) egressEndpoint(w http.ResponseWriter, r *http.Request, rec record) {
	tok := rec.EgressToken
	if tok == "" {
		updated, ok := s.update(rec.ID, func(r *record) {
			if r.EgressToken == "" {
				r.EgressToken = newToken()
			}
		})
		if !ok {
			writeErr(w, http.StatusNotFound, "SANDBOX::NOT_FOUND", rec.ID+" was deleted")
			return
		}

		tok = updated.EgressToken
	}

	writeJSON(w, http.StatusOK, endpointJSON{
		Endpoint: r.Host + "/v1/sandboxes/" + rec.ID + "/egress",
		Headers:  map[string]string{egressAuthHeader: tok},
	})
}
