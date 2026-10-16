package credvault

// What the vault accepts, and why each refusal is a refusal.
//
// These rules are upstream's, from the Credential Vault store at the commit in test/osb/UPSTREAM
// (components/egress/pkg/credentialvault/vault.go). They are reimplemented rather than loosened
// because a client writes its bindings against the reference implementation: a rule sbx does not
// enforce is a sandbox that behaves differently from the server the caller tested against, and
// for a credential binding "differently" means a secret sent somewhere it was not meant to go.
//
// Two rules are sbx's own, at the end. A binding on a host the sandbox's egress policy denies is
// refused, because the filter would answer that request with 403 and the vault would have reported
// protection for a request that never leaves. And two bindings that could both match one request
// are refused as a pair, because exactly one applies at request time (Apply) - a set that cannot
// be applied is a set that should not be written.

import (
	"regexp"
	"sort"
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
// every credential a binding names exists, every host a binding names is one the sandbox's egress
// policy lets it reach, and no two bindings cover the same request.
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

	return oneBindingPerRequest(bindings)
}

// oneBindingPerRequest refuses two bindings that could both cover one request.
//
// Exactly one binding applies at request time, so an overlapping pair is a vault that cannot be
// used: whichever request falls in the overlap is refused with ErrAmbiguousBinding, on a tunnel,
// long after the write that caused it. Found here instead, the caller hears it from the PATCH that
// did it and can say which binding they meant.
//
// It is checked over the whole candidate rather than the bindings being added, because a PATCH
// that adds one binding can collide with one that was already there, and a PATCH that replaces one
// can collide with a binding nobody touched.
func oneBindingPerRequest(bindings map[string]Binding) error {
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}

	sort.Strings(names)

	for i, a := range names {
		for _, b := range names[i+1:] {
			if overlap, how := bindingsOverlap(bindings[a], bindings[b]); overlap {
				return invalid("bindings %q and %q both cover %s: exactly one binding applies to "+
					"a request, so a request in the overlap would be refused rather than "+
					"credentialled. Narrow one of them - a different host, scheme, method or "+
					"path - or fold the two into one binding", a, b, how)
			}
		}
	}

	return nil
}

// bindingsOverlap reports whether one request could match both bindings, and names an example of
// such a request for the refusal. Both bindings are normalized, so every field is populated.
//
// A request matches on four things at once (matches in apply.go), so the bindings overlap only if
// all four do. Any one field that cannot agree keeps them apart, which is what makes the rule
// cheap to satisfy: a second binding on the same host needs only a method or a path the first does
// not claim.
func bindingsOverlap(a, b Binding) (overlap bool, example string) {
	scheme, ok := firstCommon(a.Match.Schemes, b.Match.Schemes)
	if !ok {
		return false, ""
	}

	method, ok := firstCommon(a.Match.Methods, b.Match.Methods)
	if !ok {
		return false, ""
	}

	host, ok := overlappingHost(a.Match.Hosts, b.Match.Hosts)
	if !ok {
		return false, ""
	}

	path, ok := overlappingPath(a.Match.Paths, b.Match.Paths)
	if !ok {
		return false, ""
	}

	return true, method + " " + scheme + "://" + host + path
}

// firstCommon is a value both sets hold, in the first set's order so the message does not depend on
// which binding was named first.
func firstCommon(first, second []string) (string, bool) {
	for _, v := range first {
		if contains(second, v) {
			return v, true
		}
	}

	return "", false
}

// overlappingHost is a hostname both host sets match, or false.
func overlappingHost(first, second []string) (string, bool) {
	for _, x := range first {
		for _, y := range second {
			if host, ok := hostPatternsOverlap(x, y); ok {
				return host, true
			}
		}
	}

	return "", false
}

// hostPatternsOverlap is a name both patterns match, for the two forms normalizeHost allows: an
// FQDN, or a leading "*." over a domain.
//
// The case worth being careful about is a wildcard covering another binding's exact host:
// "*.example.com" and "api.example.com" are different strings that both match api.example.com, so
// comparing the patterns for equality would miss the overlap entirely. A wildcard does not cover
// its own domain, because matchesHost requires a label in front of it - so "*.example.com" and
// "example.com" do not overlap, and that is the one pair that reads like it should.
func hostPatternsOverlap(x, y string) (string, bool) {
	wildX, wildY := strings.HasPrefix(x, "*."), strings.HasPrefix(y, "*.")
	domX, domY := strings.TrimPrefix(x, "*."), strings.TrimPrefix(y, "*.")

	switch {
	case !wildX && !wildY:
		return x, x == y

	// Two wildcards overlap when one domain sits under the other: a name below the deeper of the
	// two is matched by both.
	case wildX && wildY:
		switch {
		case domX == domY:
			return "host." + domX, true
		case strings.HasSuffix(domX, "."+domY):
			return "host." + domX, true
		case strings.HasSuffix(domY, "."+domX):
			return "host." + domY, true
		}

		return "", false

	case wildX:
		return y, strings.HasSuffix(y, "."+domX)

	default:
		return x, strings.HasSuffix(x, "."+domY)
	}
}

// overlappingPath is a path both path sets match, or false.
func overlappingPath(first, second []string) (string, bool) {
	for _, x := range first {
		for _, y := range second {
			if path, ok := pathPatternsOverlap(x, y); ok {
				return path, true
			}
		}
	}

	return "", false
}

// pathPatternsOverlap is a path both patterns match, for the spec's one glob: a trailing "*" is a
// prefix, anything else is exact (matchesPath).
//
// Two prefixes overlap when either contains the other, and the deeper one is the example - every
// path under it is under both. The default "/*" is a prefix of every pattern, which is why two
// bindings that name no path at all on one host always collide.
func pathPatternsOverlap(x, y string) (string, bool) {
	preX, wildX := strings.TrimSuffix(x, "*"), strings.HasSuffix(x, "*")
	preY, wildY := strings.TrimSuffix(y, "*"), strings.HasSuffix(y, "*")

	switch {
	case wildX && wildY:
		switch {
		case strings.HasPrefix(preX, preY):
			return preX, true
		case strings.HasPrefix(preY, preX):
			return preY, true
		}

		return "", false

	case wildX:
		return y, strings.HasPrefix(y, preX)

	case wildY:
		return x, strings.HasPrefix(x, preY)

	default:
		return x, x == y
	}
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
