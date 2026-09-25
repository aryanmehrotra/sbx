package egress

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// A network policy in OpenSandbox's own shape, so the lifecycle API can hand its request body
// straight to this package and hand the answer straight back.
//
// The shape and every semantic below were checked against OpenSandbox release-1.1.0 - the tag
// the compatibility suite pins - and each is cited where it is implemented, because "what the
// upstream does" is a claim that ages silently and a citation is how the next person re-checks
// it against a newer tag instead of trusting this comment.
//
// This file is compiled into the filter container verbatim (see BuildContext), so it imports
// the standard library only and stays in a package whose clause is the first line.

// ActionAllow and ActionDeny are the only two actions a rule or a default can carry.
const (
	ActionAllow = "allow"
	ActionDeny  = "deny"
)

// CodeInvalidRequest is the code a malformed policy is refused with, in OpenSandbox's error
// shape ({code, message}, specs/sandbox-lifecycle.yml ErrorResponse).
const CodeInvalidRequest = "INVALID_REQUEST"

// Policy is OpenSandbox's NetworkPolicy (specs/sandbox-lifecycle.yml, components.schemas
// .NetworkPolicy): a default action and an ordered list of rules.
type Policy struct {
	DefaultAction string `json:"defaultAction"`
	Egress        []Rule `json:"egress"`
}

// Rule is OpenSandbox's NetworkRule. Target is an FQDN ("example.com"), a wildcard
// ("*.example.com" - subdomains only, not the apex), an IP, or a CIDR.
type Rule struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

// Error is a refusal in OpenSandbox's error shape, so the lifecycle layer can serialise it as-is.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func invalid(format string, a ...any) *Error {
	return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf(format, a...)}
}

// MarshalJSON writes an empty rule list as [] rather than null. The schema says array, and a
// client that ranges over "egress" should not have to know Go's nil-slice encoding.
func (p Policy) MarshalJSON() ([]byte, error) {
	type plain Policy

	if p.Egress == nil {
		p.Egress = []Rule{}
	}

	return json.Marshal(plain(p))
}

// DenyAll is the policy an empty body resets to - upstream's DefaultDenyPolicy
// (components/egress/pkg/policy/policy.go:40) and its empty-POST reset
// (components/egress/policy_server.go handlePost, raw == "").
func DenyAll() Policy { return Policy{DefaultAction: ActionDeny, Egress: []Rule{}} }

// ParsePolicy decodes and validates a policy body.
//
// Empty, "null" and "{}" are deny-all, and an omitted defaultAction is deny: both are upstream's
// ParsePolicy (components/egress/pkg/policy/policy.go:65-79). Fields the schema does not declare
// are refused, because the schema says additionalProperties: false and a misnamed
// "default" silently becoming deny is the kind of surprise a security control should not
// have.
func ParsePolicy(raw []byte) (Policy, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		return DenyAll(), nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()

	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, invalid("invalid network policy: %v. The shape is "+
			`{"defaultAction":"allow|deny","egress":[{"action":"allow|deny","target":"example.com"}]}`, err)
	}

	return p.Normalize()
}

// ParseRules decodes a PATCH body: a non-empty array of rules. Empty is refused, as upstream
// refuses it (components/egress/policy_server.go handlePatch, "invalid patch rules: empty array").
func ParseRules(raw []byte) ([]Rule, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	dec.DisallowUnknownFields()

	var rules []Rule
	if err := dec.Decode(&rules); err != nil {
		return nil, invalid("invalid patch rules: %v. The body is a JSON array of "+
			`{"action":"allow|deny","target":"..."}`, err)
	}

	if len(rules) == 0 {
		return nil, invalid("invalid patch rules: empty array - send at least one rule")
	}

	for i := range rules {
		r, err := rules[i].normalize()
		if err != nil {
			return nil, err
		}

		rules[i] = r
	}

	return rules, nil
}

// ParseTargets decodes a DELETE body: a non-empty array of target strings.
func ParseTargets(raw []byte) ([]string, error) {
	var targets []string
	if err := json.Unmarshal(bytes.TrimSpace(raw), &targets); err != nil {
		return nil, invalid("invalid delete targets: %v. The body is a JSON array of targets, "+
			`like ["bad.example.com","*.blocked.org"]`, err)
	}

	if len(targets) == 0 {
		return nil, invalid("invalid delete targets: empty array - name at least one target")
	}

	return targets, nil
}

// Normalize validates a policy and returns it in canonical form: actions lower-cased, targets
// trimmed, an omitted default made explicit.
func (p Policy) Normalize() (Policy, error) {
	out := Policy{DefaultAction: strings.ToLower(strings.TrimSpace(p.DefaultAction))}

	switch out.DefaultAction {
	case "":
		out.DefaultAction = ActionDeny // policy.go:134-137
	case ActionAllow, ActionDeny:
	default:
		return Policy{}, invalid("defaultAction %q is not valid - it is %q or %q",
			p.DefaultAction, ActionAllow, ActionDeny)
	}

	out.Egress = make([]Rule, 0, len(p.Egress))

	for _, r := range p.Egress {
		n, err := r.normalize()
		if err != nil {
			return Policy{}, err
		}

		out.Egress = append(out.Egress, n)
	}

	return out, nil
}

func (r Rule) normalize() (Rule, error) {
	out := Rule{Action: strings.ToLower(strings.TrimSpace(r.Action)), Target: strings.TrimSpace(r.Target)}

	// An empty action is deny, not an error: upstream's normalizePolicy does exactly this
	// (components/egress/pkg/policy/policy.go:141-144), and refusing what the reference
	// implementation accepts would break a client that works against it.
	if out.Action == "" {
		out.Action = ActionDeny
	}

	if out.Action != ActionAllow && out.Action != ActionDeny {
		return Rule{}, invalid("rule for %q: action %q is not valid - it is %q or %q",
			r.Target, r.Action, ActionAllow, ActionDeny)
	}

	if out.Target == "" {
		return Rule{}, invalid("egress target cannot be empty")
	}

	if _, err := netip.ParseAddr(out.Target); err == nil {
		return out, nil
	}

	if _, err := netip.ParsePrefix(out.Target); err == nil {
		return out, nil
	}

	// Stricter than upstream, deliberately. Upstream files anything that is not an IP or CIDR
	// as a domain, so "https://example.com" or "example.com:443" becomes a rule that can never
	// match anything. For an allow that is a confusing outage; for a DENY it is a hole that
	// looks like a control, which is the one failure a policy must not have.
	if err := validDomain(out.Target); err != nil {
		return Rule{}, invalid("rule target %q: %v", out.Target, err)
	}

	return out, nil
}

func validDomain(t string) error {
	name := strings.TrimSuffix(strings.TrimPrefix(t, "*."), ".")

	if strings.Contains(t, "://") || strings.ContainsAny(t, "/:@ ") {
		return fmt.Errorf("a target is a bare host, a *.wildcard, an IP or a CIDR - not a URL " +
			"or host:port; write it as \"example.com\"")
	}

	if name == "" || strings.Contains(name, "*") {
		return fmt.Errorf("a wildcard is only a leading \"*.\" followed by a domain, " +
			"like \"*.example.com\"")
	}

	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("%q is not a valid domain name", t)
		}

		for _, c := range label {
			ok := c == '-' || c == '_' || (c >= '0' && c <= '9') ||
				(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			if !ok {
				return fmt.Errorf("%q is not a valid domain name", t)
			}
		}
	}

	return nil
}

// Merge applies a PATCH: incoming rules come first, in their own order, and replace any
// existing rule for the same target; within the patch the first rule for a target wins; the
// existing rules for other targets follow in their order; the default is kept.
//
// That is upstream's mergeEgressRules (components/egress/policy_utils.go:61-86), which the
// lifecycle spec defers to ("the same semantics as the sandbox-side egress service PATCH
// endpoint"). Targets compare case-insensitively (mergeKey, policy_utils.go:115-120). Because
// matching is first-match, "first" is also "highest priority", which is what makes a PATCH that
// denies a host already allowed by a wildcard take effect.
func (p Policy) Merge(rules []Rule) Policy {
	out := Policy{DefaultAction: p.DefaultAction, Egress: make([]Rule, 0, len(rules)+len(p.Egress))}
	seen := map[string]bool{}

	for _, list := range [][]Rule{rules, p.Egress} {
		for _, r := range list {
			k := strings.ToLower(r.Target)
			if seen[k] {
				continue
			}

			seen[k] = true
			out.Egress = append(out.Egress, r)
		}
	}

	return out
}

// Remove drops the rules for the given targets, compared case-insensitively; a target that is
// not present is ignored and the default is kept - upstream's removeRulesByTarget
// (components/egress/policy_utils.go:91-112). It reports how many rules went.
func (p Policy) Remove(targets []string) (Policy, int) {
	drop := map[string]bool{}

	for _, t := range targets {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			drop[t] = true
		}
	}

	out := Policy{DefaultAction: p.DefaultAction, Egress: make([]Rule, 0, len(p.Egress))}

	for _, r := range p.Egress {
		if !drop[strings.ToLower(r.Target)] {
			out.Egress = append(out.Egress, r)
		}
	}

	return out, len(p.Egress) - len(out.Egress)
}

// Mode is the summary upstream reports beside a policy: allow_all, deny_all, or enforcing -
// modeFromPolicy (components/egress/policy_utils.go:143-154).
func (p Policy) Mode() string {
	switch {
	case len(p.Egress) == 0 && p.DefaultAction == ActionAllow:
		return "allow_all"
	case len(p.Egress) == 0:
		return "deny_all"
	default:
		return "enforcing"
	}
}

// Hash identifies a policy's content, so two copies can be compared without comparing lists -
// the daemon uses it to notice that a filter is enforcing something other than what it should.
func (p Policy) Hash() string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)

	return hex.EncodeToString(h[:8])
}

// EnforcementMode is what sbx reports in the enforcementMode field. Upstream reports its own
// backends there ("dns", "dns+nft"); sbx's is a filtering proxy on a bridge with no route out.
const EnforcementMode = "proxy"

// Status is OpenSandbox's PolicyStatusResponse.
type Status struct {
	Status          string  `json:"status,omitempty"`
	Mode            string  `json:"mode,omitempty"`
	EnforcementMode string  `json:"enforcementMode,omitempty"`
	Reason          string  `json:"reason,omitempty"`
	Policy          *Policy `json:"policy,omitempty"`
}

// StatusOf is the status response for a policy that is in force.
func StatusOf(p Policy, reason string) Status {
	return Status{Status: "ok", Mode: p.Mode(), EnforcementMode: EnforcementMode, Reason: reason, Policy: &p}
}

// FromAllowList converts an egress_allow list into a policy with the meaning it always had:
// deny everything else, and each entry permits the host AND its subdomains. The second half is
// why each domain becomes two rules - upstream's "example.com" is exact and its "*.example.com"
// excludes the apex (components/egress/pkg/policy/policy.go:238-254), and an allow-list whose
// "openai.com" stopped permitting api.openai.com on upgrade would break every spec that has one.
// A port on an entry is dropped, as it always was; an IP stays an IP rule.
func FromAllowList(allow []string) Policy {
	p := Policy{DefaultAction: ActionDeny, Egress: []Rule{}}
	seen := map[string]bool{}

	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			p.Egress = append(p.Egress, Rule{Action: ActionAllow, Target: t})
		}
	}

	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if h, _, err := net.SplitHostPort(a); err == nil {
			a = h
		}

		if a = strings.TrimSuffix(a, "."); a == "" {
			continue
		}

		add(a)

		if _, err := netip.ParseAddr(a); err == nil {
			continue
		}

		if _, err := netip.ParsePrefix(a); err == nil {
			continue
		}

		if !strings.HasPrefix(a, "*.") {
			add("*." + a)
		}
	}

	return p
}

// compiled is a policy ready to be asked about: domain rules indexed for first-match, address
// rules split into deny and allow sets.
type compiled struct {
	p Policy

	// exact and wild map a pattern to the index of the FIRST rule naming it. Evaluation picks
	// the lowest index among every pattern that matches - upstream's compiled domain index
	// (components/egress/pkg/policy/domain_index.go:31-100), i.e. first match in list order.
	exact map[string]compiledRule
	wild  map[string]compiledRule // key is the suffix with its leading dot: ".example.com"

	deny, allow []netip.Prefix
}

type compiledRule struct {
	index  int
	action string
}

func compile(p Policy) *compiled {
	c := &compiled{p: p, exact: map[string]compiledRule{}, wild: map[string]compiledRule{}}

	for i, r := range p.Egress {
		var pfx netip.Prefix

		if a, err := netip.ParseAddr(r.Target); err == nil {
			pfx = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		} else if n, err := netip.ParsePrefix(r.Target); err == nil {
			pfx = unmapPrefix(n.Masked())
		} else {
			t := strings.ToLower(strings.TrimSuffix(r.Target, "."))
			m, key := c.exact, t

			if strings.HasPrefix(t, "*.") {
				m, key = c.wild, t[1:]
			}

			if _, dup := m[key]; !dup {
				m[key] = compiledRule{index: i, action: r.Action}
			}

			continue
		}

		if r.Action == ActionAllow {
			c.allow = append(c.allow, pfx)
		} else {
			c.deny = append(c.deny, pfx)
		}
	}

	return c
}

func unmapPrefix(p netip.Prefix) netip.Prefix {
	if !p.Addr().Is4In6() || p.Bits() < 96 {
		return p
	}

	return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
}

// domain returns the action of the first rule, in list order, whose pattern matches host.
func (c *compiled) domain(host string) (string, bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	best, matched := c.exact[host]

	for cursor := host; ; {
		dot := strings.IndexByte(cursor, '.')
		if dot < 0 {
			break
		}

		if r, ok := c.wild[cursor[dot:]]; ok && (!matched || r.index < best.index) {
			best, matched = r, true
		}

		cursor = cursor[dot+1:]
	}

	return best.action, matched
}

func contains(set []netip.Prefix, a netip.Addr) bool {
	for _, p := range set {
		if p.Contains(a) {
			return true
		}
	}

	return false
}

// hostLocal reports an address that means "the machine the filter runs on" rather than
// anywhere the sandbox could otherwise reach: loopback, link-local (which includes cloud
// metadata at 169.254.169.254), unspecified and multicast.
//
// This is sbx's addition, not upstream's, and it exists because of where sbx's filter sits.
// Upstream enforces inside the sandbox's own network namespace, where 127.0.0.1 is the sandbox
// itself. sbx's filter is a proxy on the host or in a container beside the sandbox, so
// "CONNECT 127.0.0.1:2375" through it would reach the FILTER's loopback - a docker socket, a
// database on the laptop - which the workload could never have reached on its own. A
// default-allow policy must not turn the proxy into a door onto its own host. An explicit
// allow rule for the address or a range containing it still opens it, because then somebody
// asked for exactly that.
func hostLocal(a netip.Addr) bool {
	return a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast()
}

// allowsAddr decides a destination given as an IP literal: address deny rules first, then
// address allow rules, then the default.
//
// Deny before allow regardless of list order is upstream's, not a choice made here: address
// rules are compiled into nftables sets and the chain drops @deny_v4 before it accepts
// @allow_v4 (components/egress/pkg/nftables/manager.go:281-286).
func (c *compiled) allowsAddr(a netip.Addr) bool {
	a = a.Unmap()

	switch {
	case contains(c.deny, a):
		return false
	case contains(c.allow, a):
		return true
	case hostLocal(a):
		return false
	default:
		return c.p.DefaultAction == ActionAllow
	}
}

// deniesResolved reports whether an address a permitted hostname resolved to is nonetheless
// refused - the DNS-rebinding guard. A name allowed by a domain rule must not become a way into
// a range an address rule denies, and upstream has the same property for the same reason: its
// resolved-address allow set is consulted after the deny sets
// (components/egress/pkg/nftables/manager.go:281-284, deny_v4 before dyn_allow_v4).
func (c *compiled) deniesResolved(a netip.Addr) bool {
	a = a.Unmap()

	if contains(c.deny, a) {
		return true
	}

	return hostLocal(a) && !contains(c.allow, a)
}

// allowsName decides a hostname before resolution: the first matching domain rule, else the
// default. Under a deny default a name no rule mentions is refused even if it would resolve
// into an allowed CIDR - upstream answers such a name's DNS query with the default, so it never
// resolves there either (components/egress/pkg/policy/policy.go:82-105).
func (c *compiled) allowsName(host string) bool {
	if action, ok := c.domain(host); ok {
		return action == ActionAllow
	}

	return c.p.DefaultAction == ActionAllow
}
