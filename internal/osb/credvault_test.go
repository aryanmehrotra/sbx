package osb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/credvault"
	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// hostedFilter is a provider whose sandboxes get the filter the daemon runs itself: a gateway, and
// no container holding it. That is the only arrangement a credential vault is allowed in.
type hostedFilter struct{ *fakeDocker }

func (f hostedFilter) List(ctx context.Context, sandbox string) ([]provider.Unit, error) {
	return decorate(f.fakeDocker, ctx, sandbox, "")
}

// containerFilter is the other arrangement: the daemon could not bind the bridge gateway, so the
// filter runs as a container on the bridge and publishes the activity endpoint whose address is
// the EgressStat label. Every Mac looks like this.
type containerFilter struct{ *fakeDocker }

func (f containerFilter) List(ctx context.Context, sandbox string) ([]provider.Unit, error) {
	return decorate(f.fakeDocker, ctx, sandbox, "127.0.0.1:20998")
}

// vmBackedDocker is a containerFilter host that can also say so before a sandbox exists, which is
// what the docker provider does by probing docker's own bridge gateway. Every Mac is one.
type vmBackedDocker struct{ containerFilter }

func (vmBackedDocker) HostsEgressFilters() error { return provider.ErrFilterNeedsNativeDocker }

func decorate(p *fakeDocker, ctx context.Context, sandbox, stat string) ([]provider.Unit, error) {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return nil, err
	}

	for i := range units {
		units[i].EgressGateway = "198.18.44.1"
		units[i].EgressStat = stat
	}

	return units, nil
}

// withVault is a server that can serve the Credential Vault API, on a host whose filter the daemon
// hosts. No feature switch: the opt-in is credentialProxy on the create.
func withVault(t *testing.T, reg *credvault.Registry) option {
	t.Helper()
	return func(h *harness, o *Options) {
		o.Provider = hostedFilter{h.p}
		o.Credentials = reg
	}
}

// credentialProxyCreate is a create that asks for a credential proxy and the policy it needs.
func credentialProxyCreate() map[string]any {
	body := minimalCreate()
	body["credentialProxy"] = map[string]any{"enabled": true}
	body["networkPolicy"] = map[string]any{
		"defaultAction": "deny",
		"egress":        []map[string]string{{"action": "allow", "target": "api.example.com"}},
	}

	return body
}

// credentialProxy needs no feature switch. It sat behind SBX_FEATURES=credential-vault while the
// vault recorded bindings and the filter added nothing to a request; the filter adds the credential
// now, so a finished feature must not still answer 501 until an operator finds an environment
// variable. The opt-in is the field on the create, per sandbox.
//
// SBX_FEATURES is emptied deliberately: the point is that nothing in the environment is required.
func TestCredentialProxyNeedsNoFeatureSwitch(t *testing.T) {
	t.Setenv("SBX_FEATURES", "")

	h := newHarness(t, withVault(t, credvault.NewRegistry()))

	var sb sandboxJSON

	resp := h.do("POST", "/v1/sandboxes", credentialProxyCreate(), &sb)
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("credentialProxy with no SBX_FEATURES = %d %s, want 202", resp.StatusCode, raw)
	}

	rec, ok := h.srv.snapshot(sb.ID)
	if !ok || !rec.CredentialProxy {
		t.Fatal("the sandbox was accepted but not recorded as having a credential proxy")
	}
}

// Off is a legal state, and it was not before: `present` treated {"enabled": false} as a request
// for the feature and answered 501. The schema gives `enabled` a default of false, so {} and an
// explicit false both mean off and both have to be accepted - the reference server reads them
// that way, and a client written against it would otherwise be refused here.
func TestCredentialProxyOffIsAccepted(t *testing.T) {
	t.Setenv("SBX_FEATURES", "")

	h := newHarness(t)

	for _, raw := range []string{`{"enabled": false}`, `{}`, `null`} {
		body := minimalCreate()
		body["credentialProxy"] = json.RawMessage(raw)

		var sb sandboxJSON

		resp := h.do("POST", "/v1/sandboxes", body, &sb)
		if resp.StatusCode != http.StatusAccepted {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("credentialProxy %s = %d %s, want 202: it says off, which is a legal thing "+
				"to say", raw, resp.StatusCode, raw)
		}

		rec, ok := h.srv.snapshot(sb.ID)
		if !ok || rec.CredentialProxy {
			t.Fatalf("credentialProxy %s was recorded as enabled", raw)
		}
	}
}

// A field this server would read as "off" when the caller meant "on" is the one failure a security
// feature must not have. Each of these is refused with the field named, rather than accepted and
// quietly ignored.
func TestACredentialProxyThisServerCannotReadIsRefused(t *testing.T) {
	h := newHarness(t)

	for name, raw := range map[string]string{
		"a quoted boolean":    `{"enabled": "true"}`,
		"a misspelled field":  `{"enabledd": true}`,
		"an unknown field":    `{"enabled": true, "requireTls": true}`,
		"a string instead":    `"enabled"`,
		"a boolean instead":   `true`,
		"a number for enable": `{"enabled": 1}`,
	} {
		body := minimalCreate()
		body["credentialProxy"] = json.RawMessage(raw)

		var out map[string]any

		resp := h.do("POST", "/v1/sandboxes", body, &out)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s (%s) = %d, want 400", name, raw, resp.StatusCode)

			continue
		}

		if msg, _ := json.Marshal(out); !strings.Contains(string(msg), "credentialProxy") {
			t.Errorf("%s was refused without naming the field: %s", name, msg)
		}
	}
}

// The credential is added by the filter, and the filter is what a policy creates. Upstream refuses
// this combination too, with the same reason.
func TestCredentialProxyNeedsANetworkPolicy(t *testing.T) {
	h := newHarness(t, withVault(t, credvault.NewRegistry()))

	body := minimalCreate()
	body["credentialProxy"] = map[string]any{"enabled": true}

	var out map[string]any

	resp := h.do("POST", "/v1/sandboxes", body, &out)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("credentialProxy with no networkPolicy = %d, want 400", resp.StatusCode)
	}

	if msg, _ := json.Marshal(out); !strings.Contains(string(msg), "networkPolicy") {
		t.Fatalf("the refusal does not say what to add: %s", msg)
	}
}

// A host that can never honour credentialProxy is a 400 on the create, not a sandbox the caller
// has to create and then poll to be told it is Failed. The question is the provider's and it needs
// no sandbox, so it is asked while the request can still be refused.
func TestCredentialProxyIsRefusedBeforeTheSandboxIsCreated(t *testing.T) {
	h := newHarness(t, func(h *harness, o *Options) {
		o.Provider = vmBackedDocker{containerFilter{h.p}}
		o.Credentials = credvault.NewRegistry()
	})

	resp := h.do("POST", "/v1/sandboxes", credentialProxyCreate(), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("credentialProxy on a host that cannot host a filter = %d, want 400: the caller "+
			"should not have to create a sandbox to learn this", resp.StatusCode)
	}

	msg := h.errOf(resp).Message

	for _, want := range []string{"credentialProxy", "VM-backed docker", "colima",
		"Docker Desktop", "native", "doctor"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q, so it names neither the cause nor the fix: %s", want, msg)
		}
	}

	// Nothing was created, which is the point: a 400 is the whole answer.
	var list struct {
		Items []struct{ ID string } `json:"items"`
	}

	h.do("GET", "/v1/sandboxes", nil, &list)

	if len(list.Items) != 0 {
		t.Errorf("the refused create left %d sandbox(es) behind", len(list.Items))
	}
}

// The custody rule, at the door it has to hold at: a host where the filter runs as a container
// cannot hold this sandbox's private key, so the create is refused by name rather than reported
// Running with a credential proxy it can never have. This is the structural half - the provider
// here cannot answer before a sandbox exists, so the refusal reads its report after one does.
func TestCredentialProxyIsRefusedWhereTheFilterIsAContainer(t *testing.T) {
	h := newHarness(t, func(h *harness, o *Options) {
		o.Provider = containerFilter{h.p}
		o.Credentials = credvault.NewRegistry()
	})

	got := h.create(credentialProxyCreate())
	if got.Status.State != stateFailed {
		t.Fatalf("state = %s, want Failed: a sandbox that cannot hold a credential vault must "+
			"not be reported as having one", got.Status.State)
	}

	msg := got.Status.Message

	for _, want := range []string{"container", "198.18.44.1", "VM-backed", "native"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q, so it names neither the cause nor the "+
				"fix: %s", want, msg)
		}
	}

	if got.Status.Reason != "credential_proxy_unavailable" {
		t.Errorf("reason = %q, want credential_proxy_unavailable", got.Status.Reason)
	}
}

// And where the daemon does host the filter, the create is accepted and the flag is recorded - in
// the record, which is where the API's own state lives. The vault is not: it holds a private key,
// and that file is 0600 on disk and survives a restart.
func TestCredentialProxyIsAcceptedWhereTheDaemonHostsTheFilter(t *testing.T) {
	h := newHarness(t, withVault(t, credvault.NewRegistry()))

	got := h.create(credentialProxyCreate())
	if got.Status.State != stateRunning {
		t.Fatalf("state = %s (%s), want Running", got.Status.State, got.Status.Message)
	}

	rec, ok := h.srv.snapshot(got.ID)
	if !ok || !rec.CredentialProxy {
		t.Fatalf("credentialProxy was not recorded: %+v", rec)
	}

	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `"credentialProxy":true`) {
		t.Fatalf("the record does not carry the flag: %s", raw)
	}
}

// The whole API, through the door the SDK uses: endpoints/18080 plus /credential-vault, with the
// per-sandbox egress credential. The value goes in and never comes out.
func TestTheCredentialVaultAPIServesTheSpecsRoutes(t *testing.T) {
	reg := credvault.NewRegistry()
	h := newHarness(t, withVault(t, reg), func(_ *harness, o *Options) { o.Key = "k" })
	key := []string{"OPEN-SANDBOX-API-KEY", "k"}

	var sb sandboxJSON
	h.do("POST", "/v1/sandboxes", credentialProxyCreate(), &sb, key...)
	h.waitStateKeyed(sb.ID, key)

	// The daemon opens the vault beside the filter it hosts; here that is this line.
	reg.Open(sb.ID, func() (egress.Policy, bool) {
		p, _ := egress.ParsePolicy([]byte(`{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com"}]}`))

		return p, true
	})

	var ep endpointJSON
	if resp := h.do("GET", "/v1/sandboxes/"+sb.ID+"/endpoints/18080", nil, &ep, key...); resp.StatusCode != 200 {
		t.Fatalf("endpoint 18080 = %d", resp.StatusCode)
	}

	auth := []string{egressAuthHeader, ep.Headers[egressAuthHeader]}
	vault := "/v1/sandboxes/" + sb.ID + "/egress/credential-vault"

	// Nothing there yet.
	if resp := h.do("GET", vault, nil, nil, auth...); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET before a create = %d, want 404", resp.StatusCode)
	}

	create := `{"credentials":[{"name":"api","source":{"type":"inline","value":"sk-live-secret"}}],
		"bindings":[{"name":"api","match":{"hosts":["api.example.com"]},
		"auth":{"type":"bearer","credential":"api"}}]}`

	var st credvault.State
	if resp := h.do("POST", vault, create, &st, auth...); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST = %d, want 201", resp.StatusCode)
	}

	if st.Revision != 1 || len(st.Bindings) != 1 || st.Bindings[0].Auth.Type != "bearer" {
		t.Fatalf("state = %+v", st)
	}

	// A second create is a conflict, not a replacement.
	if resp := h.do("POST", vault, create, nil, auth...); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a second POST = %d, want 409", resp.StatusCode)
	}

	// Every read route, and none of them carries the value.
	for _, path := range []string{vault, vault + "/credentials", vault + "/credentials/api",
		vault + "/bindings", vault + "/bindings/api"} {
		resp := h.do("GET", path, nil, nil, auth...)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if strings.Contains(string(body), "sk-live-secret") {
			t.Fatalf("GET %s returned the credential value: %s", path, body)
		}
	}

	// PATCH rotates the value against the revision it read.
	patch := `{"expectedRevision":1,"credentials":{"replace":[{"name":"api",
		"source":{"type":"inline","value":"sk-rotated"}}]}}`

	var after credvault.State
	if resp := h.do("PATCH", vault, patch, &after, auth...); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200", resp.StatusCode)
	}

	if after.Revision != 2 {
		t.Fatalf("revision = %d, want 2", after.Revision)
	}

	// The same PATCH again is a conflict: the revision has moved.
	if resp := h.do("PATCH", vault, patch, nil, auth...); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a stale expectedRevision = %d, want 409", resp.StatusCode)
	}

	// A field the spec does not have is refused rather than dropped.
	if resp := h.do("PATCH", vault, `{"credential":{"delete":["api"]}}`, nil, auth...); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown field = %d, want 400", resp.StatusCode)
	}

	if resp := h.do("DELETE", vault, nil, nil, auth...); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", resp.StatusCode)
	}

	if resp := h.do("DELETE", vault, nil, nil, auth...); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a second DELETE = %d, want 404", resp.StatusCode)
	}
}

// The routes carry the per-sandbox egress credential, not execd's token, which the workload holds.
// A wrong one is 401 with the same answer a missing sandbox gets, so nobody can enumerate ids.
func TestTheCredentialVaultRoutesNeedTheEgressCredential(t *testing.T) {
	reg := credvault.NewRegistry()
	h := newHarness(t, withVault(t, reg), func(_ *harness, o *Options) { o.Key = "k" })
	key := []string{"OPEN-SANDBOX-API-KEY", "k"}

	var sb sandboxJSON
	h.do("POST", "/v1/sandboxes", credentialProxyCreate(), &sb, key...)
	h.waitStateKeyed(sb.ID, key)

	vault := "/v1/sandboxes/" + sb.ID + "/egress/credential-vault"

	if resp := h.do("GET", vault, nil, nil, egressAuthHeader, "guess"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong credential = %d, want 401", resp.StatusCode)
	}

	// No credential and no API key either.
	if resp := h.do("GET", vault, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credential at all = %d, want 401", resp.StatusCode)
	}
}

// A sandbox that did not ask for a credential proxy has no vault, and is told so rather than given
// an empty one. 412 is the spec's status for a prerequisite that is not met.
func TestAVaultIsRefusedForASandboxThatDidNotAskForOne(t *testing.T) {
	reg := credvault.NewRegistry()
	h := newHarness(t, withVault(t, reg), func(_ *harness, o *Options) { o.Key = "k" })
	key := []string{"OPEN-SANDBOX-API-KEY", "k"}

	var sb sandboxJSON
	h.do("POST", "/v1/sandboxes", minimalCreate(), &sb, key...)
	h.waitStateKeyed(sb.ID, key)

	vault := "/v1/sandboxes/" + sb.ID + "/egress/credential-vault"

	resp := h.do("GET", vault, nil, nil, key...)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("GET on a sandbox without credentialProxy = %d, want 412", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "credentialProxy") {
		t.Fatalf("the refusal does not say what to create the sandbox with: %s", body)
	}
}

// A server whose daemon does not host egress filters cannot hold a key, and says so at both doors:
// the create, and the vault routes.
func TestWithoutARegistryTheVaultIsRefusedEverywhere(t *testing.T) {
	h := newHarness(t, func(h *harness, o *Options) { o.Provider = hostedFilter{h.p}; o.Key = "k" })
	key := []string{"OPEN-SANDBOX-API-KEY", "k"}

	var out map[string]any
	if resp := h.do("POST", "/v1/sandboxes", credentialProxyCreate(), &out, key...); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("credentialProxy with no registry = %d, want 501", resp.StatusCode)
	}

	var sb sandboxJSON
	h.do("POST", "/v1/sandboxes", minimalCreate(), &sb, key...)
	h.waitStateKeyed(sb.ID, key)

	resp := h.do("GET", "/v1/sandboxes/"+sb.ID+"/egress/credential-vault", nil, nil, key...)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("the vault route with no registry = %d, want 412", resp.StatusCode)
	}
}

// The warm pool is the one path that answers a create without provisioning it, so it is the one
// way a credentialProxy sandbox could come into being without the host check above ever running.
// It cannot: credentialProxy is not a field a pool member may carry, so such a create always
// takes the cold path.
func TestACredentialProxyCreateIsNeverServedFromAPool(t *testing.T) {
	body := credentialProxyCreate()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	var req createRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}

	if poolable(raw, req) {
		t.Fatal("a create with credentialProxy was poolable: a claim answers without provisioning, " +
			"so the sandbox would be reported Running without the host ever being checked")
	}

	if why := poolBlocker(raw, req); !strings.Contains(why, "credentialProxy") {
		t.Fatalf("the pool refused it for %q, not for credentialProxy: the field this test guards "+
			"is not the one keeping it off the pool", why)
	}
}
