package credvault

import (
	"encoding/json"
	"errors"
	"fmt"
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

	return NewRegistry().Open("osb-000000000001", allowing(t, hosts...))
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

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatalf("a create after a delete was refused: %v", err)
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

// The authority is constrained to the bound hosts, so it is re-minted when those change - and not
// when they do not, because a new authority is a new certificate for the sandbox's trust store and
// rotating a secret must not invalidate it.
func TestTheAuthorityFollowsTheBoundHosts(t *testing.T) {
	v := newVault(t, "api.example.com", "other.example.com")

	if _, err := v.Create(bearerFor("api.example.com")); err != nil {
		t.Fatal(err)
	}

	first := v.CA()
	if first == nil {
		t.Fatal("a vault with a binding has no authority")
	}

	if got := strings.Join(first.Hosts(), ","); got != "api.example.com" {
		t.Fatalf("constraints = %q, want the bound host", got)
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

	// Adding a host moves it.
	if _, err := v.Patch(MutationRequest{Bindings: &BindingMutations{Add: []Binding{{
		Name:  "other",
		Match: Match{Hosts: []string{"other.example.com"}},
		Auth:  Auth{Type: authBearer, Credential: "api"},
	}}}}); err != nil {
		t.Fatal(err)
	}

	second := v.CA()
	if second == first {
		t.Fatal("a new bound host did not re-mint the authority, so it cannot sign for that host")
	}

	if got := strings.Join(second.Hosts(), ","); got != "api.example.com,other.example.com" {
		t.Fatalf("constraints = %q, want both bound hosts", got)
	}

	// And the one it replaced is dead, not merely unreferenced.
	if _, err := first.Leaf("api.example.com"); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("the replaced authority can still sign (%v): its key outlived the revision that "+
			"created it", err)
	}
}

// A vault belongs to the filter the daemon hosts, so closing that filter destroys the key. The
// identity check at the same site is why: a sandbox recreated under the same name must not inherit
// a stranger's credentials.
func TestClosingAVaultDestroysItsKey(t *testing.T) {
	r := NewRegistry()
	v := r.Open("osb-000000000001", allowing(t, "api.example.com"))

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
	fresh := r.Open("osb-000000000001", allowing(t, "api.example.com"))
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
