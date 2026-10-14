package credvault

// What the vault accepts, and why each refusal is a refusal.
//
// These rules are upstream's, from the Credential Vault store at the commit in test/osb/UPSTREAM
// (components/egress/pkg/credentialvault/vault.go). They are reimplemented rather than loosened
// because a client writes its bindings against the reference implementation: a rule sbx does not
// enforce is a sandbox that behaves differently from the server the caller tested against, and
// for a credential binding "differently" means a secret sent somewhere it was not meant to go.
//
// One rule is sbx's own, at the end: a binding on a host the sandbox's egress policy denies is
// refused, because the filter would answer that request with 403 and the vault would have reported
// protection for a request that never leaves.

import (
	"regexp"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/egress"
)

// record is one credential as the vault holds it: the name, what kind of source it came from, the
// plaintext, and the revision it was last written in.
type record struct {
	Name       string
	SourceType string
	Source     Secret
	Revision   int64
}

// The auth types the spec defines. Anything else is refused by name rather than ignored: a binding
// whose type the vault does not know is a binding that adds nothing, which looks exactly like a
// working one until the request is rejected by the host it was sent to.
const (
	authBearer        = "bearer"
	authBasic         = "basic"
	authAPIKey        = "apiKey"
	authCustomHeaders = "customHeaders"
	authPassthrough   = "passthrough"
)

// defaultMethods and defaultPaths are the spec's defaults for a match that omits them.
func defaultMethods() []string { return []string{"GET", "POST", "PUT", "PATCH", "DELETE"} }
func defaultPaths() []string   { return []string{"/*"} }

// defaultSchemes is https only. A binding that wants plaintext says so.
func defaultSchemes() []string { return []string{"https"} }

// substitutionSurfaces are where a placeholder may be replaced.
func substitutionSurfaces() []string { return []string{"path", "query", "header", "body"} }

// headerFieldName is RFC 7230's token, which is what the spec's pattern says.
var headerFieldName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+\-.^_` + "`" + `|~]+$`)

// hostnameLabel is one label of a DNS name.
var hostnameLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9\-_]{0,61}[A-Za-z0-9])?$`)

// reservedHeaderNames may not carry a credential. Every one of them is a header the proxy, the
// transport or the upstream owns: a binding that sets Host, Content-Length or X-Forwarded-For is
// not adding a credential, it is rewriting the request's framing, and the result is a request the
// upstream reads differently from the one the sandbox sent.
func reservedHeaderName(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-length", "content-type", "transfer-encoding", "connection", "upgrade",
		"te", "trailer", "proxy-authorization", "proxy-authenticate", "forwarded",
		"x-forwarded-for", "x-forwarded-host", "x-forwarded-proto":
		return true
	}

	return false
}

// normalizeCredential checks one credential and stamps it with the revision it is written in.
func normalizeCredential(c Credential, revision int64) (record, error) {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		return record{}, invalid("a credential needs a name: it is what a binding refers to")
	}

	if t := strings.TrimSpace(c.Source.Type); t != "inline" {
		return record{}, invalid("credential %q: source.type %q is not a source this vault has - "+
			"it is \"inline\", with the value in source.value", name, t)
	}

	if c.Source.Value.reveal() == "" {
		return record{}, invalid("credential %q: source.value is empty, so the binding using it "+
			"would add an empty header", name)
	}

	return record{Name: name, SourceType: "inline", Source: c.Source.Value, Revision: revision}, nil
}

// normalizeBinding checks one binding and fills in the spec's defaults.
func normalizeBinding(b Binding) (Binding, error) {
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		return Binding{}, invalid("a binding needs a name: it is what a PATCH refers to")
	}

	if err := normalizeMatch(&b.Match); err != nil {
		return Binding{}, invalid("binding %q: %v", b.Name, err)
	}

	if err := normalizeAuth(&b.Auth); err != nil {
		return Binding{}, invalid("binding %q: %v", b.Name, err)
	}

	return b, nil
}

func normalizeMatch(m *Match) error {
	if len(m.Hosts) == 0 {
		return invalid("match.hosts cannot be empty: a binding with no host is a credential with " +
			"nowhere to go")
	}

	if len(m.Schemes) == 0 {
		m.Schemes = defaultSchemes()
	}

	if len(m.Methods) == 0 {
		m.Methods = defaultMethods()
	}

	if len(m.Paths) == 0 {
		m.Paths = defaultPaths()
	}

	for i, s := range m.Schemes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "https" && s != "http" {
			return invalid("match.schemes has %q: it is \"https\" or \"http\"", s)
		}

		m.Schemes[i] = s
	}

	// Deprecated upstream and derived from the scheme here too. A standard value is accepted and
	// dropped; anything else is refused, because it names a port this vault will never match on.
	for _, p := range m.Ports {
		if p != 80 && p != 443 {
			return invalid("match.ports has %d: the port comes from the scheme (https 443, "+
				"http 80), so drop match.ports and put the scheme in match.schemes", p)
		}
	}

	m.Ports = nil

	for i, h := range m.Hosts {
		got, err := normalizeHost(h)
		if err != nil {
			return err
		}

		m.Hosts[i] = got
	}

	for i, method := range m.Methods {
		method = strings.ToUpper(strings.TrimSpace(method))
		if method == "" {
			return invalid("match.methods has a blank entry")
		}

		m.Methods[i] = method
	}

	for i, p := range m.Paths {
		p = strings.TrimSpace(p)
		if p == "" || !strings.HasPrefix(p, "/") {
			return invalid("match.paths has %q: a path pattern starts with \"/\", like \"/v1/*\"", p)
		}

		m.Paths[i] = p
	}

	dedupe(&m.Schemes)
	dedupe(&m.Hosts)
	dedupe(&m.Methods)
	dedupe(&m.Paths)

	return nil
}

// normalizeHost is a binding host: an FQDN, or a "*."-wildcard over one. Not an address, because
// the sandbox verifies a certificate issued for a name, and not a URL, because a host that is
// silently never matched is a binding that looks like protection and is not.
func normalizeHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))

	switch {
	case host == "":
		return "", invalid("match.hosts has a blank entry")
	case strings.Contains(host, "://"), strings.ContainsAny(host, "/:@ "):
		return "", invalid("match.hosts has %q: a host is a bare name, not a URL or host:port - "+
			"write it as \"api.example.com\"", host)
	}

	name := host
	if wild := strings.HasPrefix(host, "*."); wild {
		name = host[2:]
		if name == "" || strings.Contains(name, "*") {
			return "", invalid("match.hosts has %q: a wildcard is a leading \"*.\" and a domain, "+
				"like \"*.example.com\"", host)
		}
	} else if strings.Contains(host, "*") {
		return "", invalid("match.hosts has %q: a wildcard is a leading \"*.\" and a domain, "+
			"like \"*.example.com\"", host)
	}

	if !validFQDN(name) {
		return "", invalid("match.hosts has %q: a binding names an FQDN, because the certificate "+
			"the sandbox verifies is issued for a name - not an address, and not a single label",
			host)
	}

	return host, nil
}

func validFQDN(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}

	for _, label := range strings.Split(host, ".") {
		if !hostnameLabel.MatchString(label) {
			return false
		}
	}

	return true
}

func normalizeAuth(a *Auth) error {
	a.Type = strings.TrimSpace(a.Type)

	switch a.Type {
	case authBearer, authBasic:
		a.Credential = strings.TrimSpace(a.Credential)
		if a.Credential == "" {
			return invalid("auth.credential is required for %s: it names the credential whose "+
				"value is sent", a.Type)
		}
	case authAPIKey:
		a.Name = canonicalHeader(strings.TrimSpace(a.Name))
		if err := checkHeaderName(a.Name); err != nil {
			return err
		}

		a.Credential = strings.TrimSpace(a.Credential)
		if a.Credential == "" {
			return invalid("auth.credential is required for apiKey: it names the credential sent " +
				"in auth.name")
		}
	case authCustomHeaders:
		if len(a.Headers) == 0 {
			return invalid("auth.headers is required for customHeaders, with at least one entry")
		}

		seen := map[string]bool{}

		for i := range a.Headers {
			h := &a.Headers[i]
			h.Name = canonicalHeader(strings.TrimSpace(h.Name))

			if err := checkHeaderName(h.Name); err != nil {
				return err
			}

			if key := strings.ToLower(h.Name); seen[key] {
				return invalid("auth.headers names %s twice: one header carries one credential",
					h.Name)
			} else {
				seen[key] = true
			}

			if h.Credential = strings.TrimSpace(h.Credential); h.Credential == "" {
				return invalid("auth.headers entry %s needs a credential", h.Name)
			}
		}
	case authPassthrough:
		// Passthrough adds no credential of its own; it exists so a binding can carry
		// substitutions alone. The other fields are refused rather than ignored, because a
		// caller who filled them in expected them to be sent.
		switch {
		case strings.TrimSpace(a.Credential) != "":
			return invalid("auth.credential does not belong to passthrough, which adds no " +
				"header - use bearer, basic or apiKey to send a credential")
		case strings.TrimSpace(a.Name) != "":
			return invalid("auth.name does not belong to passthrough")
		case len(a.Headers) != 0:
			return invalid("auth.headers does not belong to passthrough")
		}
	default:
		return invalid("auth.type %q is not a type this vault has: it is one of %s, %s, %s, %s or %s",
			a.Type, authBearer, authBasic, authAPIKey, authCustomHeaders, authPassthrough)
	}

	return normalizeSubstitutions(a.Substitutions)
}

func checkHeaderName(name string) error {
	if name == "" || !headerFieldName.MatchString(name) {
		return invalid("%q is not a header name: it is a token, like \"X-Api-Key\"", name)
	}

	if reservedHeaderName(name) {
		return invalid("%s is reserved: it frames the request rather than authenticating it, "+
			"so a credential in it would change what the upstream reads", name)
	}

	return nil
}

// canonicalHeader spells a header name the way net/http does, so two bindings cannot disagree
// about the same header by case alone.
func canonicalHeader(name string) string {
	if name == "" || !headerFieldName.MatchString(name) {
		return name
	}

	parts := strings.Split(strings.ToLower(name), "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}

	return strings.Join(parts, "-")
}

func normalizeSubstitutions(subs []Substitution) error {
	// seen keys a placeholder to the surface it was already claimed on, so the message can say
	// which earlier substitution it collides with.
	seen := map[[2]string]int{}

	for i := range subs {
		s := &subs[i]

		s.Credential = strings.TrimSpace(s.Credential)
		if s.Credential == "" {
			return invalid("substitutions[%d] needs a credential: it names the value that "+
				"replaces the placeholder", i)
		}

		if s.Placeholder == "" {
			return invalid("substitutions[%d] needs a placeholder to replace", i)
		}

		if len(s.In) == 0 {
			return invalid("substitutions[%d] needs at least one surface in `in`: one of %s", i,
				strings.Join(substitutionSurfaces(), ", "))
		}

		for j, surface := range s.In {
			surface = strings.ToLower(strings.TrimSpace(surface))

			if !contains(substitutionSurfaces(), surface) {
				return invalid("substitutions[%d].in has %q: it is one of %s", i, surface,
					strings.Join(substitutionSurfaces(), ", "))
			}

			s.In[j] = surface

			key := [2]string{s.Placeholder, surface}
			if prev, dup := seen[key]; dup {
				return invalid("substitutions[%d] replaces %q on the %s surface, which "+
					"substitutions[%d] already does: two values for one placeholder is not a "+
					"choice the vault can make", i, s.Placeholder, surface, prev)
			}

			seen[key] = i
		}
	}

	return nil
}

// validateCandidate checks a whole proposed vault, after the individual pieces are normalized:
// every credential a binding names exists, and every host a binding names is one the sandbox's
// egress policy lets it reach.
func validateCandidate(credentials map[string]record, bindings map[string]Binding, pol egress.Policy, hasPolicy bool) error {
	for _, b := range bindings {
		for _, name := range credentialsOf(b.Auth) {
			if _, ok := credentials[name]; !ok {
				return invalid("binding %q names the credential %q, which this vault does not "+
					"have: add it in the same request (credentials.add), or name one of the "+
					"credentials it already holds", b.Name, name)
			}
		}
	}

	// Upstream requires an egress policy before a binding can exist, and so does sbx: without
	// one there is no filter, so nothing would read the request and the binding would be inert.
	if len(bindings) > 0 && !hasPolicy {
		return &NotReadyError{Message: "a credential binding needs an egress policy, because the " +
			"filter that would add the credential is what a policy creates. Create the sandbox " +
			"with a networkPolicy - \"networkPolicy\": {\"defaultAction\": \"deny\", \"egress\": " +
			"[{\"action\": \"allow\", \"target\": \"api.example.com\"}]}"}
	}

	for _, b := range bindings {
		for _, h := range b.Match.Hosts {
			// A wildcard is tested through a name under it: the policy matches names, and
			// "*.example.com" is not a name anything resolves.
			probe := h
			if strings.HasPrefix(h, "*.") {
				probe = "probe." + h[2:]
			}

			if !pol.AllowsName(probe) {
				return invalid("binding %q names the host %q, which this sandbox's egress policy "+
					"denies: the request would be refused with 403 before any credential was "+
					"added. Allow it first - PATCH /v1/sandboxes/{id}/networkpolicy with "+
					"[{\"action\": \"allow\", \"target\": %q}] - or drop the host from the binding",
					b.Name, h, h)
			}
		}
	}

	return nil
}

// credentialsOf is every credential name an auth refers to, including its substitutions.
func credentialsOf(a Auth) []string {
	var out []string

	switch a.Type {
	case authBearer, authBasic, authAPIKey:
		out = append(out, a.Credential)
	case authCustomHeaders:
		for _, h := range a.Headers {
			out = append(out, h.Credential)
		}
	}

	for _, s := range a.Substitutions {
		out = append(out, s.Credential)
	}

	return out
}

// sanitizeAuth is a binding's auth with every credential name dropped. The apiKey header name
// stays: it is not a secret, and it is how a caller tells two apiKey bindings apart.
func sanitizeAuth(a Auth) *AuthMetadata {
	meta := &AuthMetadata{Type: a.Type}
	if a.Type == authAPIKey {
		meta.Name = a.Name
	}

	return meta
}

func dedupe(in *[]string) {
	seen := map[string]bool{}
	out := (*in)[:0]

	for _, v := range *in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}

	*in = out
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}

	return false
}
