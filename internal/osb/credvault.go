package osb

// The OpenSandbox Credential Vault API, served from here.
//
// Upstream's door to it is the egress sidecar inside the sandbox, which the SDK reaches through
// endpoints/18080 plus /credential-vault. sbx has no sidecar - the filter is the daemon's - so
// that endpoint points back at this listener and these routes answer there, beside /policy.
//
// What they refuse, and why each refusal is a refusal rather than a quiet no:
//
//   - A sandbox created without credentialProxy has no vault, and never gets one by accident.
//   - A sandbox whose filter runs as a CONTAINER has no vault either, whatever it asked for. The
//     certificate authority's key and every secret would live in that container, on the sandbox's
//     own bridge, configured over an endpoint with no redaction. Refused by name, with the reason
//     and the fix (internal/credvault explains the custody argument in full) - at the create,
//     with a 400, where the provider can say so before a sandbox exists (provider.FilterHoster),
//     and again on the provider's own report once it has created one.
//   - A credential value is write-only. It goes in and never comes out, of any route here.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/aryanmehrotra/sbx/internal/credvault"
)

const (
	// featureCredentialVault gates this whole surface. It is off by default because the vault
	// records credentials and bindings and the filter does not yet add them to requests: a
	// sandbox with a vault still needs its own credentials, and the honest default is the 501
	// that says so (docs/ROADMAP.md).
	featureCredentialVault = "credential-vault"

	credentialVaultGate = "it is a preview feature - turn it on with " +
		"SBX_FEATURES=credential-vault on `sbx serve`, and read docs/ROADMAP.md for what is " +
		"still missing from it"
)

// credentialVaultRoutes is the sidecar-shaped path these routes live on, for the authentication
// check in authed: the per-sandbox egress credential is what the SDK sends here, not the API key.
var credentialVaultRoutes = regexp.MustCompile(`^/v1/sandboxes/[^/]+/egress/credential-vault(/.*)?$`)

// credentialVault serves POST, GET, PATCH and DELETE /credential-vault.
func (s *Server) credentialVault(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vault(w, r)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.vaultState(w, v)
	case http.MethodPost:
		var req credvault.CreateRequest
		if !readVaultBody(w, r, &req) {
			return
		}

		st, err := v.Create(req)
		if err != nil {
			vaultErr(w, err)
			return
		}

		writeJSON(w, http.StatusCreated, st)
	case http.MethodPatch:
		var req credvault.MutationRequest
		if !readVaultBody(w, r, &req) {
			return
		}

		st, err := v.Patch(req)
		if err != nil {
			vaultErr(w, err)
			return
		}

		writeJSON(w, http.StatusOK, st)
	case http.MethodDelete:
		if err := v.Delete(); err != nil {
			vaultErr(w, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST, PATCH, DELETE")
		http.Error(w, r.Method+" is not a method /credential-vault answers", http.StatusMethodNotAllowed)
	}
}

// credentialVaultCredentials serves the sanitized credential metadata: the list, and one by name.
func (s *Server) credentialVaultCredentials(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vault(w, r)
	if !ok {
		return
	}

	if name := r.PathValue("name"); name != "" {
		got, err := v.Credential(name)
		if err != nil {
			vaultErr(w, err)
			return
		}

		writeJSON(w, http.StatusOK, got)

		return
	}

	got, err := v.CredentialList()
	if err != nil {
		vaultErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, got)
}

// credentialVaultBindings is the same for bindings.
func (s *Server) credentialVaultBindings(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vault(w, r)
	if !ok {
		return
	}

	if name := r.PathValue("name"); name != "" {
		got, err := v.Binding(name)
		if err != nil {
			vaultErr(w, err)
			return
		}

		writeJSON(w, http.StatusOK, got)

		return
	}

	got, err := v.BindingList()
	if err != nil {
		vaultErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, got)
}

func (s *Server) vaultState(w http.ResponseWriter, v *credvault.Vault) {
	st, err := v.State()
	if err != nil {
		vaultErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, st)
}

// vault authenticates the call and finds the sandbox's vault, answering the refusal itself when
// there is not one. Every reason a sandbox has no vault is a separate sentence with the fix in it.
func (s *Server) vault(w http.ResponseWriter, r *http.Request) (*credvault.Vault, bool) {
	rec, ok := s.egressAuthed(w, r)
	if !ok {
		return nil, false
	}

	if s.credentials == nil {
		notReady(w, "this sbx serve has no credential vault: it needs a daemon that hosts egress "+
			"filters itself. Start it with `sbx serve --osb-addr 127.0.0.1:8080` on a host whose "+
			"docker is native")

		return nil, false
	}

	if !rec.CredentialProxy {
		notReady(w, fmt.Sprintf("%s was created without a credential proxy, so it has no vault. "+
			"Create a sandbox with \"credentialProxy\": {\"enabled\": true} and a networkPolicy "+
			"to use this API", rec.ID))

		return nil, false
	}

	v, ok := s.credentials.Lookup(rec.ID)
	if !ok {
		// The sandbox asked for a proxy and has none, which has one likely cause worth naming
		// precisely rather than guessing at: its filter is a container, so this daemon cannot
		// hold its key.
		why := s.filterCanHoldAVault(r.Context(), rec.ID)
		if why == nil {
			why = fmt.Errorf("%s has no credential vault yet: its egress filter has not been "+
				"started. Wait for the sandbox to reach Running and try again, and if `sbx serve` "+
				"was restarted, send the POST again - a vault is held in memory and does not "+
				"survive a restart", rec.ID)
		}

		notReady(w, why.Error())

		return nil, false
	}

	return v, true
}

// egressAuthed is the sidecar routes' authentication, shared by /policy and /credential-vault: the
// per-sandbox egress credential from endpoints/18080, or - with no such header - the API key check
// in authed, which has already passed, because the operator can do this through the API too.
func (s *Server) egressAuthed(w http.ResponseWriter, r *http.Request) (record, bool) {
	id := r.PathValue("id")

	rec, ok := s.snapshot(id)

	if got := r.Header.Get(egressAuthHeader); got != "" {
		// One answer for "no such sandbox" and "wrong credential": this path skipped the API key,
		// so a 404 here would let anybody enumerate sandbox ids.
		if !ok || rec.EgressToken == "" || !sameToken(got, rec.EgressToken) {
			http.Error(w, "invalid "+egressAuthHeader+": fetch it from GET /v1/sandboxes/{id}/endpoints/18080 with the API key",
				http.StatusUnauthorized)

			return record{}, false
		}
	}

	if !validID(id) || !ok {
		http.Error(w, "no such sandbox", http.StatusNotFound)

		return record{}, false
	}

	return rec, true
}

// filterCanHoldAVault is the one host question a credential vault turns on: is this sandbox's
// egress filter one the daemon hosts itself?
//
// Only a hosted filter can have a vault, because only the daemon's own process should hold the
// certificate authority's key. Where the daemon cannot bind the sandbox's bridge gateway - a
// VM-backed docker, which is every Mac - the same filter runs as a container on the bridge, and a
// key in there is a key in a container on the sandbox's own network, configured over an endpoint
// with no redaction. So that sandbox is refused by name instead of quietly running a weaker
// arrangement.
//
// The answer comes from the provider's own report of what it created, not from a probe of our own:
// which form of filter a sandbox got is the provider's decision, and asking it is the negotiation
// every other capability here uses (docs/DECISIONS.md, "Capabilities are negotiated, not stubbed").
// Nil means the filter can hold one.
func (s *Server) filterCanHoldAVault(ctx context.Context, id string) error {
	units, err := s.p.List(ctx, id)
	if err != nil {
		return nil
	}

	var filtered bool

	for _, u := range units {
		if u.EgressGateway == "" {
			continue
		}

		filtered = true

		if u.EgressStat != "" {
			return fmt.Errorf("%s cannot have a credential vault on this machine: its egress "+
				"filter runs as a container, because `sbx serve` cannot bind the sandbox's "+
				"bridge gateway %s.\n"+
				"That is what a VM-backed docker looks like - colima and Docker Desktop keep the "+
				"bridge inside the VM, while `sbx serve` runs out here - and a credential vault "+
				"there would hold this sandbox's private key and secrets inside a container on "+
				"its own bridge. Check it with `ifconfig | grep %s`, and run the daemon on a host "+
				"whose docker is native to use credentialProxy",
				id, u.EgressGateway, u.EgressGateway)
		}
	}

	if !filtered {
		return fmt.Errorf("%s has no egress filter, so it cannot have a credential vault: the "+
			"credential is added by the filter. Create the sandbox with a networkPolicy", id)
	}

	return nil
}

// refuseWithoutAHostedFilter is the create-time half of the same rule, called once the provider has
// created the sandbox and so decided which form of filter it gets. Loud and early: a sandbox
// reported Running with a credentialProxy it can never have is the silent weaker deployment this
// refusal exists to prevent.
//
// It is the structural guarantee, not the error anyone should normally see: a provider that can
// answer provider.FilterHoster has already turned this host into a 400 at the create (validate in
// provision.go). This still runs, because the form of filter is the provider's own report and a
// host question asked before the sandbox existed is a prediction about it.
func (s *Server) refuseWithoutAHostedFilter(ctx context.Context, pl plan) error {
	if !pl.credentialProxy {
		return nil
	}

	return s.filterCanHoldAVault(ctx, pl.rec.ID)
}

// readVaultBody decodes a request body, refusing a field the spec does not have rather than
// dropping it: an ignored field in a credential binding is protection the caller believes they
// configured and did not.
func readVaultBody(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "reading the body: "+err.Error(), http.StatusBadRequest)

		return false
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		http.Error(w, "the body is not a Credential Vault request: "+err.Error()+
			". The schema is specs/egress-api.yaml in OpenSandbox release-1.1.0",
			http.StatusBadRequest)

		return false
	}

	return true
}

// vaultErr answers a vault failure with the status the spec gives it, in plain text: these routes
// are the sidecar's, and the sidecar's error responses are text/plain (specs/egress-api.yaml).
func vaultErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), credvault.HTTPStatus(err))
}

// notReady is 412: the request is well formed and the sandbox exists, and this machine or this
// sandbox cannot hold a credential vault.
func notReady(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusPreconditionFailed)
}

// sameToken compares in constant time, like the egress credential check it came from.
func sameToken(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
