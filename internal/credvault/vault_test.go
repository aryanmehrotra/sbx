package credvault

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/egress"
)

// allowing is a policy that permits the hosts named and denies everything else, which is the shape
// a credential vault is meant to be used with.
func allowing(t *testing.T, hosts ...string) func() (egress.Policy, bool) {
	t.Helper()

	p := egress.Policy{DefaultAction: "deny"}
	for _, h := range hosts {
		p.Egress = append(p.Egress, egress.Rule{Action: "allow", Target: h})
	}

	norm, err := p.Normalize()
	if err != nil {
		t.Fatal(err)
	}

	return func() (egress.Policy, bool) { return norm, true }
}

func newVault(t *testing.T, hosts ...string) *Vault {
	t.Helper()

	return openVault(t, NewRegistry(), "osb-000000000001", allowing(t, hosts...))
}

// openVault is the order the daemon works in, which a vault's own rules depend on: the authority is
// minted while the sandbox is created, so that it reaches the trust store, and the vault adopts
// that one when the filter comes up. A vault never mints its own.
func openVault(t *testing.T, r *Registry, sandbox string, policy func() (egress.Policy, bool)) *Vault {
	t.Helper()

	pol, _ := policy()

	if _, err := r.Certificate(sandbox, pol); err != nil {
		t.Fatal(err)
	}

	return r.Open(sandbox, policy)
}

// bearerFor is a vault holding one credential and one bearer binding for host.
func bearerFor(host string) CreateRequest {
	return CreateRequest{
		Credentials: []Credential{{Name: "api", Source: InlineSource{Type: "inline", Value: "sk-live-secret"}}},
		Bindings: []Binding{{
			Name:  "api",
			Match: Match{Hosts: []string{host}},
			Auth:  Auth{Type: authBearer, Credential: "api"},
		}},
	}
}

// The credential value goes in and never comes out: not in the create response, not in a later GET,
// not in the metadata routes. This is the whole promise of the feature - the sandbox does not hold
// the secret and neither does anything a caller can read.
func TestACredentialValueIsNeverReturned(t *testing.T) {
	v := newVault(t, "api.example.com")

	st, err := v.Create(bearerFor("api.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := v.State()
	if err != nil {
		t.Fatal(err)
	}

	creds, _ := v.CredentialList()
	binds, _ := v.BindingList()
	one, _ := v.Credential("api")
	bind, _ := v.Binding("api")

	for _, shape := range []any{st, got, creds, binds, one, bind} {
		raw, err := json.Marshal(shape)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(string(raw), "sk-live-secret") {
			t.Fatalf("a credential value reached a response body: %s", raw)
		}
	}

	if st.Revision != 1 || len(st.Credentials) != 1 || st.Credentials[0].SourceType != "inline" {
		t.Fatalf("state = %+v, want revision 1 and one inline credential", st)
	}
}

// The redaction is the type, so the value cannot be printed by the paths that are not responses
// either: a log line, an error, a %#v dump of a request somebody kept.
func TestASecretCannotBePrinted(t *testing.T) {
	s := Secret("sk-live-secret")

	for _, got := range []string{
		s.String(),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%s", s),
		fmt.Sprintf("%#v", s),
		fmt.Sprint(InlineSource{Type: "inline", Value: s}),
	} {
		if strings.Contains(got, "sk-live-secret") {
			t.Errorf("a secret printed as %q", got)
		}
	}

	raw, err := json.Marshal(InlineSource{Type: "inline", Value: s})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(raw), "sk-live-secret") {
		t.Errorf("a secret marshalled as %s", raw)
	}

	// And it still arrives: a redaction that broke the input would be a vault that holds nothing.
	var in InlineSource
	if err := json.Unmarshal([]byte(`{"type":"inline","value":"sk-live-secret"}`), &in); err != nil {
		t.Fatal(err)
	}

	if in.Value.reveal() != "sk-live-secret" {
		t.Fatalf("the value did not survive the round trip: %q", in.Value.reveal())
	}
}

// A binding on a host the sandbox cannot reach is refused. Accepting it would report protection for
// a request the filter answers with 403, which is the kind of quiet disagreement a security
// feature must not have.
func TestABindingOnADeniedHostIsRefused(t *testing.T) {
	v := newVault(t, "api.example.com")

	_, err := v.Create(bearerFor("api.elsewhere.com"))
	if err == nil {
		t.Fatal("a binding for a host the egress policy denies was accepted")
	}

	if got := HTTPStatus(err); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got)
	}

	for _, want := range []string{"api.elsewhere.com", "403", "networkpolicy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// Without a policy there is no filter, so a binding would be inert. That is a prerequisite the host
// has not met rather than a bad request, and the spec gives it its own status.
func TestABindingNeedsAnEgressPolicy(t *testing.T) {
	v := NewRegistry().Open("osb-000000000001", func() (egress.Policy, bool) {
		return egress.Policy{}, false
	})

	_, err := v.Create(bearerFor("api.example.com"))
	if got := HTTPStatus(err); got != http.StatusPreconditionFailed {
		t.Fatalf("status = %d (%v), want 412", got, err)
	}

	if !strings.Contains(err.Error(), "networkPolicy") {
		t.Errorf("the refusal does not say how to get a filter: %v", err)
	}
}

// expectedRevision is what stops two writers dropping each other's bindings. S4's rotation leans on
// it, so a mismatch has to be a conflict and not a silent overwrite.
func TestAStaleExpectedRevisionIsAConflict(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	stale := int64(1)

	st, err := v.Patch(MutationRequest{ExpectedRevision: &stale, Credentials: &CredentialMutations{
		Replace: []Credential{{Name: "api", Source: InlineSource{Type: "inline", Value: "sk-rotated"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if st.Revision != 2 || st.Credentials[0].Revision != 2 {
		t.Fatalf("state = %+v, want the vault and the rotated credential both at revision 2", st)
	}

	// The same request again: the revision has moved on.
	_, err = v.Patch(MutationRequest{ExpectedRevision: &stale, Credentials: &CredentialMutations{
		Replace: []Credential{{Name: "api", Source: InlineSource{Type: "inline", Value: "sk-again"}}},
	}})

	var conflict *RevisionError
	if !errors.As(err, &conflict) {
		t.Fatalf("a stale expectedRevision gave %v, want a conflict", err)
	}

	if got := HTTPStatus(err); got != http.StatusConflict {
		t.Fatalf("status = %d, want 409", got)
	}

	if conflict.Expected != 1 || conflict.Current != 2 {
		t.Fatalf("conflict = %+v, want expected 1 and current 2", conflict)
	}
}

// A PATCH is applied whole or not at all: a half-applied credential change is a sandbox
// authenticating to one host and not another, with nothing in the response to say which.
func TestARefusedPatchChangesNothing(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	_, err := v.Patch(MutationRequest{
		Credentials: &CredentialMutations{Delete: []string{"api"}},
		Bindings: &BindingMutations{Add: []Binding{{
			Name:  "second",
			Match: Match{Hosts: []string{"api.example.com"}},
			Auth:  Auth{Type: authBearer, Credential: "api"},
		}}},
	})
	if err == nil {
		t.Fatal("a patch that deleted the credential its new binding names was accepted")
	}

	st, err := v.State()
	if err != nil {
		t.Fatal(err)
	}

	if st.Revision != 1 || len(st.Credentials) != 1 || len(st.Bindings) != 1 {
		t.Fatalf("state = %+v, want the vault untouched at revision 1", st)
	}
}

// The POST is not a replacement. A client that posts twice has lost track of what it configured,
// and discarding the first set of bindings is the wrong guess to make with credentials.
func TestASecondCreateIsAConflictAndDeleteMakesRoomForOne(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	if _, err := v.Create(bearerFor("api.example.com")); !errors.Is(err, ErrExists) {
		t.Fatalf("a second create gave %v, want ErrExists", err)
	}

	if got := HTTPStatus(ErrExists); got != http.StatusConflict {
		t.Fatalf("ErrExists maps to %d, want 409", got)
	}

	if err := v.Delete(); err != nil {
		t.Fatal(err)
	}

	if _, err := v.State(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("state after delete = %v, want ErrNotFound", err)
	}

	if got := HTTPStatus(ErrNotFound); got != http.StatusNotFound {
		t.Fatalf("ErrNotFound maps to %d, want 404", got)
	}

	ca := v.CA()

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatalf("a create after a delete was refused: %v", err)
	}

	// The same authority, because the sandbox's trust store still holds its certificate and
	// nothing can replace that while the sandbox runs.
	if v.CA() != ca {
		t.Fatal("the create after a delete signs with a different authority: every bound host " +
			"would fail its handshake against the certificate the sandbox was given")
	}

	if _, err := ca.Leaf("api.example.com"); err != nil {
		t.Fatalf("the authority cannot sign after a delete and a create: %v", err)
	}
}

// What the vault accepts is upstream's rule set, because a client writes its bindings against the
// reference implementation. Each of these is refused there too.
func TestTheRefusalsUpstreamMakesAreTheRefusalsHere(t *testing.T) {
	cred := func(name string) []Credential {
		return []Credential{{Name: name, Source: InlineSource{Type: "inline", Value: "v"}}}
	}

	for name, req := range map[string]CreateRequest{
		"a binding with no host": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Auth: Auth{Type: authBearer, Credential: "api"}}}},
		"a host written as a URL": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"https://api.example.com"}},
				Auth: Auth{Type: authBearer, Credential: "api"}}}},
		"a host that is an address": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"10.0.0.1"}},
				Auth: Auth{Type: authBearer, Credential: "api"}}}},
		"a scheme that is not http or https": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}, Schemes: []string{"ftp"}},
				Auth: Auth{Type: authBearer, Credential: "api"}}}},
		"a port the scheme does not derive": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}, Ports: []int{8443}},
				Auth: Auth{Type: authBearer, Credential: "api"}}}},
		"a path that is not a path": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}, Paths: []string{"v1/*"}},
				Auth: Auth{Type: authBearer, Credential: "api"}}}},
		"an auth type that does not exist": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}},
				Auth: Auth{Type: "oauth", Credential: "api"}}}},
		"a bearer with no credential": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}}, Auth: Auth{Type: authBearer}}}},
		"an apiKey in a reserved header": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}},
				Auth: Auth{Type: authAPIKey, Name: "Host", Credential: "api"}}}},
		"customHeaders with no headers": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}},
				Auth: Auth{Type: authCustomHeaders}}}},
		"customHeaders naming one header twice": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}}, Auth: Auth{
				Type:    authCustomHeaders,
				Headers: []HeaderEntry{{Name: "X-Key", Credential: "api"}, {Name: "x-key", Credential: "api"}}}}}},
		"passthrough carrying a credential": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}},
				Auth: Auth{Type: authPassthrough, Credential: "api"}}}},
		"a binding naming a credential that does not exist": {Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}},
				Auth: Auth{Type: authBearer, Credential: "nope"}}}},
		"a credential with no value": {Credentials: []Credential{
			{Name: "api", Source: InlineSource{Type: "inline"}}}},
		"a credential from a source that does not exist": {Credentials: []Credential{
			{Name: "api", Source: InlineSource{Type: "vault", Value: "v"}}}},
		"two substitutions for one placeholder on one surface": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}}, Auth: Auth{
				Type: authPassthrough, Substitutions: []Substitution{
					{Credential: "api", Placeholder: "__KEY__", In: []string{"header"}},
					{Credential: "api", Placeholder: "__KEY__", In: []string{"header", "body"}},
				}}}}},
		"a substitution with no surface": {Credentials: cred("api"), Bindings: []Binding{
			{Name: "b", Match: Match{Hosts: []string{"api.example.com"}}, Auth: Auth{
				Type:          authPassthrough,
				Substitutions: []Substitution{{Credential: "api", Placeholder: "__KEY__"}}}}}},
		"two credentials with one name": {Credentials: []Credential{
			{Name: "api", Source: InlineSource{Type: "inline", Value: "a"}},
			{Name: "api", Source: InlineSource{Type: "inline", Value: "b"}}}},
	} {
		if _, err := newVault(t, "api.example.com").Create(req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The spec's defaults, filled in so a caller reading the state back sees what the binding actually
// matches rather than the blanks it sent.
func TestAMatchGetsTheSpecsDefaults(t *testing.T) {
	v := newVault(t, "api.example.com")

	st, err := v.Create(bearerFor("api.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	m := st.Bindings[0].Match

	if strings.Join(m.Schemes, ",") != "https" {
		t.Errorf("schemes = %v, want https", m.Schemes)
	}

	if strings.Join(m.Methods, ",") != "GET,POST,PUT,PATCH,DELETE" {
		t.Errorf("methods = %v, want the spec's five", m.Methods)
	}

	if strings.Join(m.Paths, ",") != "/*" {
		t.Errorf("paths = %v, want /*", m.Paths)
	}

	// A standard port is accepted and dropped, because the port comes from the scheme.
	if _, err := v.Patch(MutationRequest{Bindings: &BindingMutations{Replace: []Binding{{
		Name:  "api",
		Match: Match{Hosts: []string{"api.example.com"}, Ports: []int{443}},
		Auth:  Auth{Type: authBearer, Credential: "api"},
	}}}}); err != nil {
		t.Fatal(err)
	}

	after, _ := v.State()
	if len(after.Bindings[0].Match.Ports) != 0 {
		t.Errorf("ports = %v, want dropped", after.Bindings[0].Match.Ports)
	}
}

// Bound is the one question the data path asks. It answers on the name and the port, because a
// CONNECT carries nothing else.
func TestBoundAnswersOnTheNameAndThePort(t *testing.T) {
	v := newVault(t, "api.example.com", "*.wild.example.com")

	if v.Bound("api.example.com", "443") {
		t.Fatal("an empty vault claimed a destination")
	}

	if _, err := v.Create(CreateRequest{
		Credentials: []Credential{{Name: "api", Source: InlineSource{Type: "inline", Value: "v"}}},
		Bindings: []Binding{
			{Name: "exact", Match: Match{Hosts: []string{"api.example.com"}},
				Auth: Auth{Type: authBearer, Credential: "api"}},
			{Name: "wild", Match: Match{Hosts: []string{"*.wild.example.com"}, Schemes: []string{"http"}},
				Auth: Auth{Type: authBearer, Credential: "api"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		host, port string
		want       bool
	}{
		{"api.example.com", "443", true},
		{"API.Example.com.", "443", true},
		{"api.example.com", "80", false},   // the binding is https
		{"api.example.com", "8443", false}, // not a port a proxy reads HTTP on
		{"sub.api.example.com", "443", false},
		{"one.wild.example.com", "80", true},
		{"one.wild.example.com", "443", false},
		{"wild.example.com", "80", false}, // a wildcard is subdomains, not the apex
	} {
		if got := v.Bound(c.host, c.port); got != c.want {
			t.Errorf("Bound(%q, %q) = %v, want %v", c.host, c.port, got, c.want)
		}
	}

	// A vault is what the filter's seam takes, and nothing else needs to be written for that.
	var _ egress.CredentialSource = v

	if err := v.Delete(); err != nil {
		t.Fatal(err)
	}

	if v.Bound("api.example.com", "443") {
		t.Fatal("a deleted vault still claimed a destination")
	}
}

// The authority is constrained to the hosts the POLICY allows, and never re-minted while the
// sandbox lives. That is what makes a delivered trust store stay true: it is read once by whatever
// runs in the sandbox - Node, Python and Go all cache it at startup - so an authority that moved
// when a binding was added would leave a long-running agent trusting one that no longer exists,
// and every swap after that would fail its handshake with nothing to say why.
//
// It is not wider than it needs to be: a binding cannot name a host the policy does not already
// allow, so the policy's names are a superset of the bindings' names by construction.
func TestTheAuthorityIsStableAndCoversEveryHostThePolicyAllows(t *testing.T) {
	v := newVault(t, "api.example.com", "other.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	first := v.CA()
	if first == nil {
		t.Fatal("a vault with a binding has no authority")
	}

	// Both policy hosts, although only one is bound: the second may be bound later, and that
	// must not need a new certificate.
	if got := strings.Join(first.Hosts(), ","); got != "api.example.com,other.example.com" {
		t.Fatalf("constraints = %q, want every host the policy allows", got)
	}

	// Rotating the value leaves the authority alone.
	if _, err := v.Patch(MutationRequest{Credentials: &CredentialMutations{
		Replace: []Credential{{Name: "api", Source: InlineSource{Type: "inline", Value: "sk-rotated"}}},
	}}); err != nil {
		t.Fatal(err)
	}

	if v.CA() != first {
		t.Fatal("rotating a credential re-minted the authority, which invalidates the certificate " +
			"the sandbox already trusts")
	}

	// Adding a host does NOT move it, which is the whole point. Before this, it did.
	if _, err := v.Patch(MutationRequest{Bindings: &BindingMutations{Add: []Binding{{
		Name:  "other",
		Match: Match{Hosts: []string{"other.example.com"}},
		Auth:  Auth{Type: authBearer, Credential: "api"},
	}}}}); err != nil {
		t.Fatal(err)
	}

	if v.CA() != first {
		t.Fatal("binding a new host re-minted the authority: the sandbox's trust store now names " +
			"a certificate that has been destroyed, and every swap after this fails its handshake")
	}

	// And it can sign for the host that was bound second, because the policy already covered it.
	if _, err := first.Leaf("other.example.com"); err != nil {
		t.Fatalf("the authority cannot sign for a host bound after it was minted: %v", err)
	}
}

// The certificate exists as soon as the vault is opened, before anything is written to it. The
// sandbox's trust store is delivered when the sandbox is created, and the API can only create a
// vault afterwards - so an authority that waited for the first write would have nothing to deliver.
func TestTheAuthorityExistsBeforeAnythingIsWrittenToTheVault(t *testing.T) {
	v := newVault(t, "api.example.com")

	ca := v.CA()
	if ca == nil {
		t.Fatal("a freshly opened vault has no authority, so there is no certificate to put in " +
			"the sandbox's trust store at create")
	}

	if len(ca.PEM()) == 0 {
		t.Fatal("the authority has no PEM to deliver")
	}

	// Nothing is bound yet, so nothing is terminated: the certificate existing is not the filter
	// reading anything.
	if v.Bound("api.example.com", "443") {
		t.Error("a vault with no bindings claimed a destination")
	}
}

// A policy with no names of its own has nothing to terminate TLS for, so there is no authority
// rather than one constrained to nothing.
func TestAPolicyOfAddressesAloneGetsNoAuthority(t *testing.T) {
	p := egress.Policy{DefaultAction: "deny", Egress: []egress.Rule{
		{Action: "allow", Target: "10.0.0.0/8"},
		{Action: "allow", Target: "192.0.2.7"},
	}}

	norm, err := p.Normalize()
	if err != nil {
		t.Fatal(err)
	}

	v := NewRegistry().Open("osb-000000000002", func() (egress.Policy, bool) { return norm, true })

	if v.CA() != nil {
		t.Error("a policy that allows only addresses got a certificate authority: there is no " +
			"name for a leaf to be the subject of")
	}
}

// A vault belongs to the filter the daemon hosts, so closing that filter destroys the key. The
// identity check at the same site is why: a sandbox recreated under the same name must not inherit
// a stranger's credentials.
func TestClosingAVaultDestroysItsKey(t *testing.T) {
	r := NewRegistry()
	v := openVault(t, r, "osb-000000000001", allowing(t, "api.example.com"))

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	ca := v.CA()

	// Open is idempotent: the reconcile loop calls it on every pass.
	if again := r.Open("osb-000000000001", allowing(t, "api.example.com")); again != v {
		t.Fatal("a second Open made a second vault, so the one the filter holds would be replaced " +
			"on the next discovery tick")
	}

	r.Close("osb-000000000001")

	if _, ok := r.Lookup("osb-000000000001"); ok {
		t.Fatal("the vault survived its filter")
	}

	if _, err := ca.Leaf("api.example.com"); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("the key survived the sandbox: %v", err)
	}

	// A fresh sandbox on the same name starts empty.
	fresh := openVault(t, r, "osb-000000000001", allowing(t, "api.example.com"))
	if _, err := fresh.State(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a recreated sandbox inherited a vault: %v", err)
	}
}

// The API's routes write this while the data path reads it, so every method has to hold up under
// -race with both happening at once.
func TestAVaultIsSafeUnderConcurrentReadsAndWrites(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for i := range 8 {
		wg.Add(2)

		go func(i int) {
			defer wg.Done()

			_, _ = v.Patch(MutationRequest{Credentials: &CredentialMutations{
				Replace: []Credential{{Name: "api",
					Source: InlineSource{Type: "inline", Value: Secret(fmt.Sprintf("sk-%d", i))}}},
			}})
		}(i)

		go func() {
			defer wg.Done()

			v.Bound("api.example.com", "443")
			_, _ = v.State()

			if ca := v.CA(); ca != nil {
				_, _ = ca.Leaf("api.example.com")
			}
		}()
	}

	wg.Wait()

	st, err := v.State()
	if err != nil {
		t.Fatal(err)
	}

	if st.Revision != 9 {
		t.Fatalf("revision = %d, want 9: every patch has to land exactly once", st.Revision)
	}
}

// httpsTo is a request as the filter hands one over after terminating a tunnel: absolute-form URL,
// https scheme, the CONNECT target as the host.
func httpsTo(t *testing.T, method, rawurl string, body io.Reader) *http.Request {
	t.Helper()

	r, err := http.NewRequest(method, rawurl, body)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

// This is the feature. An agent holds a fake token, or no token at all; the real one is added by
// the filter, outside the sandbox, on its way to a host a binding names. Everything else in this
// package exists to make this one call safe.
func TestApplyAddsTheRealCredentialToAMatchingRequest(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	r := httpsTo(t, http.MethodPost, "https://api.example.com/v1/messages", nil)

	// What the agent wrote: a placeholder it was given instead of the key.
	r.Header.Set("Authorization", "Bearer sk-placeholder-not-real")

	if err := v.Apply(r); err != nil {
		t.Fatalf("Apply refused a matching request: %v", err)
	}

	if got := r.Header.Get("Authorization"); got != "Bearer sk-live-secret" {
		t.Errorf("Authorization is %q, want the real credential: the swap is what this feature "+
			"is, and a request that leaves with the placeholder reaches the host as a bad call "+
			"by the agent", got)
	}
}

// Set, not added. A placeholder the agent wrote must not survive beside the real one: two
// Authorization headers are one header with two values on the wire, and upstream reads the first.
func TestApplyReplacesTheAgentsOwnHeaderRatherThanAppending(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	r := httpsTo(t, http.MethodGet, "https://api.example.com/v1/models", nil)
	r.Header.Add("Authorization", "Bearer sk-placeholder-not-real")

	if err := v.Apply(r); err != nil {
		t.Fatal(err)
	}

	if n := len(r.Header.Values("Authorization")); n != 1 {
		t.Fatalf("the request carries %d Authorization values, want 1: %v", n,
			r.Header.Values("Authorization"))
	}
}

// A request the bindings do not cover leaves exactly as it arrived. The vault decides what a
// permitted request carries, and a host no binding names is not its business.
func TestApplyLeavesAnUnmatchedRequestAlone(t *testing.T) {
	v := newVault(t, "api.example.com", "other.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, method, url string }{
		{"another host", http.MethodGet, "https://other.example.com/v1/models"},
		{"a method the match does not list", "TRACE", "https://api.example.com/v1/models"},
		{"plain http, when the binding defaults to https", http.MethodGet, "http://api.example.com/v1/models"},
	} {
		r := httpsTo(t, c.method, c.url, nil)
		r.Header.Set("Authorization", "Bearer sk-placeholder-not-real")

		if err := v.Apply(r); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}

		if got := r.Header.Get("Authorization"); got != "Bearer sk-placeholder-not-real" {
			t.Errorf("%s: Apply rewrote a request no binding covers (%q)", c.name, got)
		}
	}
}

// Every auth type the API accepts has an arm here. A type the vault stores and cannot apply is a
// binding that looks configured and adds nothing.
func TestApplyHandlesEveryAuthType(t *testing.T) {
	for _, c := range []struct {
		name   string
		auth   Auth
		header string
		want   string
	}{
		{"bearer", Auth{Type: authBearer, Credential: "api"}, "Authorization", "Bearer sk-live-secret"},
		{"apiKey", Auth{Type: authAPIKey, Credential: "api", Name: "X-Api-Key"}, "X-Api-Key", "sk-live-secret"},
		{
			"customHeaders",
			Auth{Type: authCustomHeaders, Headers: []HeaderEntry{{Name: "X-Token", Credential: "api"}}},
			"X-Token", "sk-live-secret",
		},
		{
			// RFC 7617: base64 of user:password, encoded here, which is the only thing that
			// makes basic different from bearer.
			"basic",
			Auth{Type: authBasic, Credential: "pair"},
			"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pw")),
		},
	} {
		v := newVault(t, "api.example.com")

		if _, err := v.Create(CreateRequest{
			Credentials: []Credential{
				{Name: "api", Source: InlineSource{Type: "inline", Value: "sk-live-secret"}},
				{Name: "pair", Source: InlineSource{Type: "inline", Value: "user:pw"}},
			},
			Bindings: []Binding{{Name: "b", Match: Match{Hosts: []string{"api.example.com"}}, Auth: c.auth}},
		}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}

		r := httpsTo(t, http.MethodGet, "https://api.example.com/v1/models", nil)

		if err := v.Apply(r); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}

		if got := r.Header.Get(c.header); got != c.want {
			t.Errorf("%s: %s is %q, want %q", c.name, c.header, got, c.want)
		}
	}
}

// The placeholder model, on every surface the spec defines. This is how an agent that writes the
// credential into a URL or a JSON body - not only a header - still never holds the real one.
func TestApplySubstitutesThePlaceholderOnEverySurface(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(CreateRequest{
		Credentials: []Credential{{Name: "api", Source: InlineSource{Type: "inline", Value: "sk-live-secret"}}},
		Bindings: []Binding{{
			Name:  "b",
			Match: Match{Hosts: []string{"api.example.com"}},
			Auth: Auth{Type: authPassthrough, Substitutions: []Substitution{{
				Credential:  "api",
				Placeholder: "__TOKEN__",
				In:          []string{"path", "query", "header", "body"},
			}}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	r := httpsTo(t, http.MethodPost,
		"https://api.example.com/v1/__TOKEN__/send?key=__TOKEN__",
		strings.NewReader(`{"token":"__TOKEN__"}`))
	r.Header.Set("X-Custom", "pre-__TOKEN__-post")

	if err := v.Apply(r); err != nil {
		t.Fatal(err)
	}

	if r.URL.Path != "/v1/sk-live-secret/send" {
		t.Errorf("path is %q, want the placeholder replaced", r.URL.Path)
	}

	if r.URL.RawQuery != "key=sk-live-secret" {
		t.Errorf("query is %q, want the placeholder replaced", r.URL.RawQuery)
	}

	if got := r.Header.Get("X-Custom"); got != "pre-sk-live-secret-post" {
		t.Errorf("header is %q, want the placeholder replaced in place", got)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != `{"token":"sk-live-secret"}` {
		t.Errorf("body is %q, want the placeholder replaced", body)
	}

	// The length has to follow the body: a credential is almost never as long as its placeholder,
	// and a body that disagrees with Content-Length is truncated upstream or hangs it.
	if r.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength is %d, want %d", r.ContentLength, len(body))
	}
}

// A binding whose credential is gone is refused, not skipped. Sending the request anyway would
// reach the real host as the agent's own unauthenticated call, which reads as the agent's bug.
func TestApplyRefusesABindingWhoseCredentialIsMissing(t *testing.T) {
	v := newVault(t, "api.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	// Reach past the API, which will not let a binding lose its credential, to the state the
	// data path has to survive.
	v.mu.Lock()
	delete(v.credentials, "api")
	v.mu.Unlock()

	r := httpsTo(t, http.MethodGet, "https://api.example.com/v1/models", nil)

	err := v.Apply(r)
	if !errors.Is(err, ErrNoSuchCredential) {
		t.Fatalf("Apply returned %v, want ErrNoSuchCredential: a request must not be sent "+
			"without the credential it was supposed to carry", err)
	}
}

// Two bindings can write one header, so which one wins cannot depend on map iteration: the same
// request would carry different credentials on different runs.
func TestApplyIsDeterministicWhenTwoBindingsWriteOneHeader(t *testing.T) {
	const runs = 20

	for range runs {
		v := newVault(t, "api.example.com")

		if _, err := v.Create(CreateRequest{
			Credentials: []Credential{
				{Name: "first", Source: InlineSource{Type: "inline", Value: "sk-first"}},
				{Name: "second", Source: InlineSource{Type: "inline", Value: "sk-second"}},
			},
			Bindings: []Binding{
				{Name: "a-first", Match: Match{Hosts: []string{"api.example.com"}},
					Auth: Auth{Type: authBearer, Credential: "first"}},
				{Name: "b-second", Match: Match{Hosts: []string{"api.example.com"}},
					Auth: Auth{Type: authBearer, Credential: "second"}},
			},
		}); err != nil {
			t.Fatal(err)
		}

		r := httpsTo(t, http.MethodGet, "https://api.example.com/v1/models", nil)

		if err := v.Apply(r); err != nil {
			t.Fatal(err)
		}

		// Binding-name order, so the later name is the one that is left.
		if got := r.Header.Get("Authorization"); got != "Bearer sk-second" {
			t.Fatalf("Authorization is %q, want the last binding by name to win every time", got)
		}
	}
}

// A vault nothing was ever written to adds nothing, rather than refusing. The filter only asks
// about a destination Bound said yes to, but a DELETE between the two is a real race.
func TestApplyOnAnEmptyVaultChangesNothing(t *testing.T) {
	v := newVault(t, "api.example.com")

	r := httpsTo(t, http.MethodGet, "https://api.example.com/v1/models", nil)

	if err := v.Apply(r); err != nil {
		t.Fatalf("Apply on a vault with no revision refused: %v", err)
	}

	if r.Header.Get("Authorization") != "" {
		t.Error("Apply added a header from a vault that holds nothing")
	}
}

// A policy widened after the sandbox was created does not widen the authority, which was minted
// from the policy as it stood then. So a binding on a host allowed later is refused at the write
// rather than accepted and then failing at the first request, when the authority declines to sign
// and the caller gets a 502 on a tunnel far from the PATCH that caused it.
func TestABindingTheAuthorityCannotSignForIsRefusedAtTheWrite(t *testing.T) {
	// The policy the vault reads is swapped under it, exactly as a PUT /networkpolicy does.
	live := policyFunc(t, "api.example.com")

	v := openVault(t, NewRegistry(), "osb-000000000003", live.read)

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	minted := v.CA()
	if minted == nil {
		t.Fatal("no authority to constrain")
	}

	// Widened after the authority was minted.
	live.set(t, "api.example.com", "late.example.com")

	_, err := v.Patch(MutationRequest{Bindings: &BindingMutations{Add: []Binding{{
		Name:  "late",
		Match: Match{Hosts: []string{"late.example.com"}},
		Auth:  Auth{Type: authBearer, Credential: "api"},
	}}}})

	if err == nil {
		t.Fatal("a binding on a host the authority cannot sign for was accepted: it would fail " +
			"at the first request instead, as a 502 on a tunnel")
	}

	if !strings.Contains(err.Error(), "may not sign for") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// And the authority is untouched by the attempt: the sandbox still trusts what it was given.
	if v.CA() != minted {
		t.Error("the refused patch moved the authority")
	}

	// The host that was always allowed still works.
	if _, err := minted.Leaf("api.example.com"); err != nil {
		t.Errorf("the authority stopped signing for its own host: %v", err)
	}
}

// policyFunc is a policy the test can change, as PUT /networkpolicy changes one under a vault.
type policy struct {
	mu  sync.RWMutex
	pol egress.Policy
}

func policyFunc(t *testing.T, hosts ...string) *policy {
	t.Helper()

	p := &policy{}
	p.set(t, hosts...)

	return p
}

func (p *policy) set(t *testing.T, hosts ...string) {
	t.Helper()

	pol := egress.Policy{DefaultAction: "deny"}
	for _, h := range hosts {
		pol.Egress = append(pol.Egress, egress.Rule{Action: "allow", Target: h})
	}

	norm, err := pol.Normalize()
	if err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	p.pol = norm
	p.mu.Unlock()
}

func (p *policy) read() (egress.Policy, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.pol, true
}

// A vault whose authority this daemon does not hold must not mint one of its own.
//
// The authority is minted while the sandbox is created and mounted into its trust store, and the
// registry holds it in memory alone. After `sbx serve` restarts that registry is empty while the
// sandbox still trusts the certificate it was given, so a second mint would leave the filter
// signing with an authority nothing in there has ever seen: every bound host fails its handshake
// with `unknown authority`, after a write that answered 201.
func TestAVaultWhoseAuthorityThisDaemonLostIsRefused(t *testing.T) {
	// No Certificate call: this is the registry of a daemon started after the sandbox was created.
	v := NewRegistry().Open("osb-000000000001", allowing(t, "api.example.com"))

	if ca := v.CA(); ca != nil {
		t.Fatalf("Open minted an authority for %v: the sandbox trusts the one it was given when it "+
			"was created, and this is not it", ca.Hosts())
	}

	if err := v.Authority(); !errors.Is(err, ErrAuthorityLost) {
		t.Fatalf("the vault reports %v, want ErrAuthorityLost: the API answers a call on it with "+
			"this, rather than an empty 404", err)
	}

	_, err := v.Create(bearerFor("api.example.com"))
	if err == nil {
		t.Fatal("the create was accepted, so every request to the bound host would fail its " +
			"handshake and the caller was told the vault was ready")
	}

	if !errors.Is(err, ErrAuthorityLost) {
		t.Fatalf("the refusal is %v, want ErrAuthorityLost", err)
	}

	if got := HTTPStatus(err); got != http.StatusPreconditionFailed {
		t.Fatalf("status = %d (%v), want 412 - the same answer every other unmet prerequisite gets",
			got, err)
	}

	// The operator can do exactly one thing about it, so the refusal has to say which.
	if !strings.Contains(err.Error(), "Create the sandbox again") {
		t.Errorf("the refusal does not say what to do next: %v", err)
	}
}

// A sandbox whose policy named no host when it was created has no authority in its trust store at
// all. Widening the policy afterwards does not put one there, so a binding on a host allowed later
// is refused at the write rather than minted for and failing at the handshake.
func TestABindingWithNoAuthorityAtAllIsRefusedAtTheWrite(t *testing.T) {
	live := policyFunc(t, "10.0.0.0/8")

	r := NewRegistry()

	// What the create did: a policy of addresses alone has no name to terminate TLS for, so
	// nothing was minted and nothing was mounted.
	pol, _ := live.read()
	if pem, err := r.Certificate("osb-000000000004", pol); err != nil || len(pem) != 0 {
		t.Fatalf("Certificate gave %d bytes of PEM (%v), want none for a policy of addresses", len(pem), err)
	}

	v := r.Open("osb-000000000004", live.read)

	live.set(t, "10.0.0.0/8", "late.example.com")

	_, err := v.Create(CreateRequest{
		Credentials: []Credential{{Name: "api", Source: InlineSource{Type: "inline", Value: "sk-live-secret"}}},
		Bindings: []Binding{{
			Name:  "late",
			Match: Match{Hosts: []string{"late.example.com"}},
			Auth:  Auth{Type: authBearer, Credential: "api"},
		}},
	})

	if err == nil {
		t.Fatal("a binding was accepted on a sandbox with no authority in its trust store: the " +
			"vault would mint one for it and the sandbox would reject every leaf it signed")
	}

	if got := HTTPStatus(err); got != http.StatusBadRequest {
		t.Fatalf("status = %d (%v), want 400: the request is the thing that cannot be honoured", got, err)
	}

	if !strings.Contains(err.Error(), "networkPolicy") {
		t.Errorf("the refusal does not say how to get an authority: %v", err)
	}
}
