package credvault

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// What Apply refuses, rather than forwarding a request without the credential it was supposed to
// carry. Each one is a refusal and not a warning: a request that reaches the real host without its
// credential fails there as a bad call by the agent, which is a worse thing to debug than a 502
// from the filter that says what went wrong.
var (
	// ErrNoSuchCredential is a binding naming a credential the vault does not hold.
	ErrNoSuchCredential = errors.New("this binding names a credential this vault does not hold")

	// ErrUnknownAuthType is an auth type Apply has no arm for. The API refuses these at write
	// time, so this means the two went out of step.
	ErrUnknownAuthType = errors.New("this binding's auth type is not one this vault can apply")

	// ErrBodyTooLarge is a body substitution on a body too big to buffer.
	ErrBodyTooLarge = errors.New("a body substitution cannot buffer a body this large")

	// errNoPort is the internal answer from splitHostPort for a host that carries none.
	errNoPort = errors.New("no port in this host")
)

// maxSubstitutionBody bounds what a body substitution will buffer.
//
// A substitution in `body` cannot be streamed: the placeholder may straddle any two reads, so the
// body has to be whole in memory before it can be replaced. That is a memory cost one sandbox can
// impose on the daemon, so it is capped, and a request over the cap is refused rather than
// forwarded with the placeholder still in it - forwarding would send the agent's fake token to the
// real host, which fails in a way that looks like a bad credential rather than a size limit.
const maxSubstitutionBody = 1 << 20 // 1 MiB

// Apply implements egress.CredentialSource: it adds this sandbox's credentials to a request the
// filter has decrypted, in place.
//
// The filter hands the request over rather than asking for a value, so no secret crosses into
// internal/egress - every file of that package is compiled into the container filter, and this one
// is not. The filter knows that a destination is bound; it never learns what with.
//
// Every binding that matches is applied, in binding-name order. The order matters because two
// bindings can write one header, and map iteration would make which one wins differ between two
// identical requests.
func (v *Vault) Apply(r *http.Request) error {
	if r == nil || r.URL == nil {
		return nil
	}

	scheme := requestScheme(r)
	host := canonicalHost(r.Host)

	if host == "" {
		host = canonicalHost(r.URL.Host)
	}

	v.mu.RLock()
	defer v.mu.RUnlock()

	if !v.exists {
		return nil
	}

	for _, name := range sortedBindingNames(v.bindings) {
		b := v.bindings[name]

		if !v.matches(b, scheme, host, r) {
			continue
		}

		if err := v.applyAuth(b, r); err != nil {
			return fmt.Errorf("binding %s: %w", b.Name, err)
		}
	}

	return nil
}

// sortedBindingNames is the deterministic order Apply walks bindings in.
func sortedBindingNames(bindings map[string]Binding) []string {
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// requestScheme is the scheme the match is tested against.
//
// Taken from the URL when the filter set one, and otherwise from whether the request arrived over
// TLS. A decrypted CONNECT carries an origin-form target with no scheme, so the filter that
// terminated the TLS is the only thing that knows, and it says so by setting URL.Scheme.
func requestScheme(r *http.Request) string {
	if s := strings.ToLower(r.URL.Scheme); s != "" {
		return s
	}

	if r.TLS != nil {
		return "https"
	}

	return "http"
}

// canonicalHost strips the port, the trailing dot and the case, which is the form Match.Hosts was
// normalized into.
func canonicalHost(host string) string {
	host = strings.TrimSpace(host)

	if host == "" {
		return ""
	}

	if h, _, err := splitHostPort(host); err == nil {
		host = h
	}

	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// splitHostPort is net.SplitHostPort, but it does not treat a bare host as an error - the common
// case here is a Host header with no port at all.
func splitHostPort(host string) (string, string, error) {
	i := strings.LastIndex(host, ":")
	if i < 0 || strings.Contains(host[i+1:], "]") {
		return host, "", errNoPort
	}

	// An IPv6 literal without a port: "[::1]" has a colon but no port after the bracket.
	if strings.HasPrefix(host, "[") && !strings.Contains(host[i:], "]:") {
		return strings.Trim(host, "[]"), "", errNoPort
	}

	return strings.Trim(host[:i], "[]"), host[i+1:], nil
}

// matches reports whether this binding covers the request: its scheme, host, method and path.
//
// Match.Ports is deliberately not consulted. The vault ignores it (the field is deprecated
// upstream and refused at write time for any value other than 80 or 443), and the port a request
// actually arrived on is the scheme's by then.
func (v *Vault) matches(b Binding, scheme, host string, r *http.Request) bool {
	if !contains(b.Match.Schemes, scheme) {
		return false
	}

	if !matchesHost(b.Match.Hosts, host) {
		return false
	}

	if !contains(b.Match.Methods, strings.ToUpper(r.Method)) {
		return false
	}

	return matchesPath(b.Match.Paths, r.URL.Path)
}

// matchesPath is the spec's path glob: a trailing `*` is a prefix, anything else is exact.
//
// The default is `/*`, so a binding that names no path matches every path of its hosts.
func matchesPath(paths []string, path string) bool {
	if path == "" {
		path = "/"
	}

	for _, p := range paths {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(path, strings.TrimSuffix(p, "*")) {
				return true
			}

			continue
		}

		if p == path {
			return true
		}
	}

	return false
}

// applyAuth writes one binding's credential into the request, then its substitutions.
//
// The header is set rather than added: a binding says what the request carries to this host, and
// appending to an Authorization the agent wrote itself would send both, which upstream reads as
// the agent's one - the fake it was given.
func (v *Vault) applyAuth(b Binding, r *http.Request) error {
	switch b.Auth.Type {
	case authBearer:
		value, err := v.reveal(b.Auth.Credential)
		if err != nil {
			return err
		}

		r.Header.Set("Authorization", "Bearer "+value)

	case authBasic:
		value, err := v.reveal(b.Auth.Credential)
		if err != nil {
			return err
		}

		// RFC 7617: the credentials are base64 of `user:password`, so the stored value is the
		// user:password pair and the encoding happens here. Encoding here is what makes basic
		// different from bearer at all - a pre-encoded value would make the two types send the
		// same bytes, and the spec would not define both.
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(value)))

	case authAPIKey:
		value, err := v.reveal(b.Auth.Credential)
		if err != nil {
			return err
		}

		r.Header.Set(b.Auth.Name, value)

	case authCustomHeaders:
		for _, h := range b.Auth.Headers {
			value, err := v.reveal(h.Credential)
			if err != nil {
				return err
			}

			r.Header.Set(h.Name, value)
		}

	case authPassthrough:
		// Adds no credential of its own. It exists so a binding can carry substitutions alone.

	default:
		// Unreachable through the API, which refuses an unknown type at write time. Refused
		// again here rather than falling through to "adds nothing", because a binding that
		// silently adds nothing looks exactly like a working one.
		return fmt.Errorf("%w: %s", ErrUnknownAuthType, b.Auth.Type)
	}

	return v.substitute(b.Auth.Substitutions, r)
}

// reveal is the credential's plaintext, by name. Callers must hold v.mu.
//
// A binding naming a credential that is not there is refused rather than skipped: the write path
// checks that every binding's credential exists, so reaching this means the two went out of step,
// and sending the request without the credential would reach the real host as the agent's own
// unauthenticated call.
func (v *Vault) reveal(name string) (string, error) {
	rec, ok := v.credentials[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNoSuchCredential, name)
	}

	return rec.Source.reveal(), nil
}

// substitute replaces each placeholder with its credential, on every surface the substitution
// names.
func (v *Vault) substitute(subs []Substitution, r *http.Request) error {
	if len(subs) == 0 {
		return nil
	}

	for _, s := range subs {
		value, err := v.reveal(s.Credential)
		if err != nil {
			return err
		}

		for _, surface := range s.In {
			if err := substituteIn(surface, s.Placeholder, value, r); err != nil {
				return err
			}
		}
	}

	return nil
}

// substituteIn replaces the placeholder on one surface.
func substituteIn(surface, placeholder, value string, r *http.Request) error {
	switch surface {
	case "path":
		// RawPath as well when it is set, because that is what is put on the wire; EscapedPath
		// falls back to Path only when RawPath is empty or inconsistent with it.
		r.URL.Path = strings.ReplaceAll(r.URL.Path, placeholder, value)

		if r.URL.RawPath != "" {
			r.URL.RawPath = strings.ReplaceAll(r.URL.RawPath, placeholder, value)
		}

	case "query":
		r.URL.RawQuery = strings.ReplaceAll(r.URL.RawQuery, placeholder, value)

	case "header":
		for name, values := range r.Header {
			for i, v := range values {
				if strings.Contains(v, placeholder) {
					r.Header[name][i] = strings.ReplaceAll(v, placeholder, value)
				}
			}
		}

	case "body":
		return substituteBody(placeholder, value, r)
	}

	return nil
}

// substituteBody replaces the placeholder in the request body, which means buffering it.
//
// Content-Length is rewritten with it: a credential is almost never the same length as the
// placeholder it replaces, and a body that disagrees with its length is either truncated by the
// upstream or hangs it waiting for bytes that will not come.
func substituteBody(placeholder, value string, r *http.Request) error {
	if r.Body == nil || placeholder == "" {
		return nil
	}

	// One byte past the cap, so a body exactly at the cap is not mistaken for one over it.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSubstitutionBody+1))
	if err != nil {
		return fmt.Errorf("reading the request body to substitute into it: %w", err)
	}

	_ = r.Body.Close()

	if len(body) > maxSubstitutionBody {
		return fmt.Errorf("%w: this request's body is over %d bytes", ErrBodyTooLarge, maxSubstitutionBody)
	}

	body = bytes.ReplaceAll(body, []byte(placeholder), []byte(value))

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprint(len(body)))

	// A buffered body is no longer chunked, and leaving the header on would describe an encoding
	// that is not there.
	r.TransferEncoding = nil

	return nil
}
