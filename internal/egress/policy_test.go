package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func mustPolicy(t *testing.T, raw string) Policy {
	t.Helper()

	p, err := ParsePolicy([]byte(raw))
	if err != nil {
		t.Fatalf("ParsePolicy(%s): %v", raw, err)
	}

	return p
}

func TestParsePolicyDefaultsAndRefusals(t *testing.T) {
	ok := []struct {
		name, raw string
		want      Policy
	}{
		{"empty body is deny-all", ``, DenyAll()},
		{"null is deny-all", `null`, DenyAll()},
		{"{} is deny-all", `{}`, DenyAll()},
		{"omitted default is deny", `{"egress":[{"action":"allow","target":"pypi.org"}]}`,
			Policy{DefaultAction: "deny", Egress: []Rule{{"allow", "pypi.org"}}}},
		{"actions are case-folded and trimmed", `{"defaultAction":" ALLOW ","egress":[{"action":"Deny","target":" x.com "}]}`,
			Policy{DefaultAction: "allow", Egress: []Rule{{"deny", "x.com"}}}},
		{"an empty action is deny, as upstream", `{"egress":[{"action":"","target":"x.com"}]}`,
			Policy{DefaultAction: "deny", Egress: []Rule{{"deny", "x.com"}}}},
		{"ip, cidr, v6 and wildcard targets", `{"egress":[{"action":"allow","target":"10.0.0.1"},{"action":"deny","target":"10.0.0.0/8"},{"action":"deny","target":"2001:db8::/32"},{"action":"allow","target":"*.example.com"}]}`,
			Policy{DefaultAction: "deny", Egress: []Rule{{"allow", "10.0.0.1"}, {"deny", "10.0.0.0/8"}, {"deny", "2001:db8::/32"}, {"allow", "*.example.com"}}}},
	}

	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			if got := mustPolicy(t, c.raw); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}

	bad := []struct{ name, raw, mention string }{
		{"unknown field", `{"default":"allow"}`, "unknown field"},
		{"bad default", `{"defaultAction":"permit"}`, "defaultAction"},
		{"bad action", `{"egress":[{"action":"block","target":"x.com"}]}`, "action"},
		{"empty target", `{"egress":[{"action":"deny","target":" "}]}`, "empty"},
		{"a url is not a target", `{"egress":[{"action":"deny","target":"https://x.com"}]}`, "not a URL"},
		{"host:port is not a target", `{"egress":[{"action":"deny","target":"x.com:443"}]}`, "host:port"},
		{"a bare star", `{"egress":[{"action":"allow","target":"*"}]}`, "wildcard"},
		{"a star mid-name", `{"egress":[{"action":"allow","target":"a.*.com"}]}`, "wildcard"},
		{"not json", `{`, "invalid network policy"},
	}

	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParsePolicy([]byte(c.raw))

			var e *Error
			if !errors.As(err, &e) || e.Code != CodeInvalidRequest {
				t.Fatalf("want an *Error with code %s, got %v", CodeInvalidRequest, err)
			}

			if !strings.Contains(e.Message, c.mention) {
				t.Errorf("message does not mention %q: %s", c.mention, e.Message)
			}
		})
	}
}

// The wire shape is upstream's, key for key: an empty rule list is [], not null.
func TestPolicyJSONMatchesTheUpstreamSchema(t *testing.T) {
	b, err := json.Marshal(DenyAll())
	if err != nil {
		t.Fatal(err)
	}

	if string(b) != `{"defaultAction":"deny","egress":[]}` {
		t.Fatalf("got %s", b)
	}

	b, _ = json.Marshal(StatusOf(mustPolicy(t, `{"egress":[{"action":"allow","target":"a.com"}]}`), ""))
	want := `{"status":"ok","mode":"enforcing","enforcementMode":"proxy","policy":{"defaultAction":"deny","egress":[{"action":"allow","target":"a.com"}]}}`

	if string(b) != want {
		t.Fatalf("status:\n got %s\nwant %s", b, want)
	}
}

// Domain rules are first-match in list order, across exact and wildcard patterns - upstream's
// compiled domain index. Order is the whole point, so every case is stated in both orders.
func TestDomainRulesAreFirstMatch(t *testing.T) {
	cases := []struct {
		policy string
		host   string
		want   bool
	}{
		// A deny listed before a broader allow carves a hole in it...
		{`{"egress":[{"action":"deny","target":"a.example.com"},{"action":"allow","target":"*.example.com"}]}`, "a.example.com", false},
		{`{"egress":[{"action":"deny","target":"a.example.com"},{"action":"allow","target":"*.example.com"}]}`, "b.example.com", true},
		// ...and after it, is shadowed by it.
		{`{"egress":[{"action":"allow","target":"*.example.com"},{"action":"deny","target":"a.example.com"}]}`, "a.example.com", true},
		// A wildcard is subdomains only, never the apex.
		{`{"egress":[{"action":"allow","target":"*.example.com"}]}`, "example.com", false},
		{`{"egress":[{"action":"allow","target":"*.example.com"}]}`, "deep.a.example.com", true},
		// Of two wildcards, the one listed first wins, not the more specific one.
		{`{"egress":[{"action":"allow","target":"*.example.com"},{"action":"deny","target":"*.a.example.com"}]}`, "x.a.example.com", true},
		{`{"egress":[{"action":"deny","target":"*.a.example.com"},{"action":"allow","target":"*.example.com"}]}`, "x.a.example.com", false},
		// Exact is exact: no suffix attacks.
		{`{"egress":[{"action":"allow","target":"example.com"}]}`, "example.com.evil.com", false},
		{`{"egress":[{"action":"allow","target":"example.com"}]}`, "notexample.com", false},
		// Case and a trailing dot do not matter.
		{`{"egress":[{"action":"allow","target":"Example.COM"}]}`, "example.com.", true},
		// Nothing matches: the default decides.
		{`{"defaultAction":"allow","egress":[{"action":"deny","target":"bad.com"}]}`, "good.com", true},
		{`{"defaultAction":"deny","egress":[{"action":"allow","target":"good.com"}]}`, "other.com", false},
	}

	for _, c := range cases {
		f := NewPolicy(mustPolicy(t, c.policy))
		if got := f.Permits(c.host); got != c.want {
			t.Errorf("%s: Permits(%q) = %v, want %v", c.policy, c.host, got, c.want)
		}
	}
}

// Address rules: deny wins over allow whatever the order, because upstream compiles them into
// nftables sets and drops @deny before it accepts @allow.
func TestAddressRulesDenyBeforeAllow(t *testing.T) {
	cases := []struct {
		policy string
		ip     string
		want   bool
	}{
		{`{"egress":[{"action":"allow","target":"10.0.0.0/8"},{"action":"deny","target":"10.1.0.0/16"}]}`, "10.1.2.3", false},
		{`{"egress":[{"action":"deny","target":"10.1.0.0/16"},{"action":"allow","target":"10.0.0.0/8"}]}`, "10.1.2.3", false},
		{`{"egress":[{"action":"allow","target":"10.0.0.0/8"},{"action":"deny","target":"10.1.0.0/16"}]}`, "10.2.0.1", true},
		{`{"defaultAction":"allow","egress":[{"action":"deny","target":"1.1.1.0/24"}]}`, "1.1.1.1", false},
		{`{"defaultAction":"allow","egress":[{"action":"deny","target":"1.1.1.0/24"}]}`, "8.8.8.8", true},
		{`{"defaultAction":"allow","egress":[{"action":"deny","target":"1.1.1.0/24"}]}`, "::ffff:1.1.1.1", false},
		{`{"defaultAction":"allow","egress":[{"action":"deny","target":"2001:db8::/32"}]}`, "2001:db8::1", false},
		{`{"defaultAction":"deny","egress":[{"action":"allow","target":"93.184.216.34"}]}`, "93.184.216.34", true},
		{`{"defaultAction":"deny"}`, "93.184.216.34", false},
		// The filter's own host is never reachable by default, even under allow...
		{`{"defaultAction":"allow"}`, "127.0.0.1", false},
		{`{"defaultAction":"allow"}`, "169.254.169.254", false},
		{`{"defaultAction":"allow"}`, "::1", false},
		{`{"defaultAction":"allow"}`, "0.0.0.0", false},
		// ...unless somebody asked for exactly that.
		{`{"egress":[{"action":"allow","target":"127.0.0.1"}]}`, "127.0.0.1", true},
	}

	for _, c := range cases {
		f := NewPolicy(mustPolicy(t, c.policy))
		if got := f.Permits(c.ip); got != c.want {
			t.Errorf("%s: Permits(%s) = %v, want %v", c.policy, c.ip, got, c.want)
		}
	}
}

// A hostname is checked twice: by name, and then every address it resolves to against the
// address rules - so a permitted name cannot be pointed into a denied range.
func TestAPermittedNameThatResolvesIntoADeniedRangeIsRefused(t *testing.T) {
	answers := map[string][]netip.Addr{
		"api.example.com":   {netip.MustParseAddr("93.184.216.34")},
		"evil.example.com":  {netip.MustParseAddr("93.184.216.35"), netip.MustParseAddr("10.0.0.5")},
		"local.example.com": {netip.MustParseAddr("127.0.0.1")},
		"corp.internal":     {netip.MustParseAddr("10.9.9.9")},
	}

	resolve := func(_ context.Context, host string) ([]netip.Addr, error) {
		if a, ok := answers[host]; ok {
			return a, nil
		}

		return nil, fmt.Errorf("no such host %s", host)
	}

	cases := []struct {
		policy, host string
		denied       bool
	}{
		{`{"egress":[{"action":"allow","target":"*.example.com"},{"action":"deny","target":"10.0.0.0/8"}]}`, "api.example.com", false},
		// One denied address among several is enough: which one the client dials is not ours to choose.
		{`{"egress":[{"action":"allow","target":"*.example.com"},{"action":"deny","target":"10.0.0.0/8"}]}`, "evil.example.com", true},
		// Rebinding onto the filter's own loopback.
		{`{"egress":[{"action":"allow","target":"*.example.com"}]}`, "local.example.com", true},
		{`{"defaultAction":"allow"}`, "local.example.com", true},
		// Default allow, with a deny range: the name is fine, its address is not.
		{`{"defaultAction":"allow","egress":[{"action":"deny","target":"10.0.0.0/8"}]}`, "corp.internal", true},
		// Under deny, an address allow does not admit a name no rule mentions (upstream never
		// resolves such a name at all).
		{`{"egress":[{"action":"allow","target":"10.0.0.0/8"}]}`, "corp.internal", true},
	}

	for _, c := range cases {
		f := NewPolicy(mustPolicy(t, c.policy))
		f.Resolve = resolve

		_, err := f.admit(context.Background(), c.host)

		var d *errDenied
		if got := errors.As(err, &d); got != c.denied {
			t.Errorf("%s: admit(%s) denied=%v (err %v), want denied=%v", c.policy, c.host, got, err, c.denied)
		}
	}
}

// PATCH, against upstream's own documented example and mergeEgressRules: incoming first, first
// of a duplicate wins, existing rules for other targets kept after, default kept.
func TestMergeIsUpstreamsPatch(t *testing.T) {
	base := Policy{DefaultAction: "allow", Egress: []Rule{{"allow", "a.com"}, {"deny", "b.com"}, {"allow", "10.0.0.0/8"}}}

	got := base.Merge([]Rule{{"deny", "A.com"}, {"allow", "c.com"}, {"allow", "a.com"}})
	want := Policy{DefaultAction: "allow", Egress: []Rule{{"deny", "A.com"}, {"allow", "c.com"}, {"deny", "b.com"}, {"allow", "10.0.0.0/8"}}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge:\n got %+v\nwant %+v", got, want)
	}

	// The effect that makes it useful: a patch denying a host a wildcard allowed takes hold.
	wild := Policy{DefaultAction: "deny", Egress: []Rule{{"allow", "*.example.com"}}}
	f := NewPolicy(wild.Merge([]Rule{{"deny", "bad.example.com"}}))

	if f.Permits("bad.example.com") || !f.Permits("ok.example.com") {
		t.Fatal("a patched deny did not take priority over the wildcard it was carved from")
	}

	if base.Egress[0].Action != "allow" {
		t.Fatal("Merge modified the policy it was called on")
	}
}

func TestRemoveByTarget(t *testing.T) {
	base := Policy{DefaultAction: "deny", Egress: []Rule{{"allow", "A.com"}, {"deny", "*.b.com"}, {"allow", "10.0.0.0/8"}}}

	got, n := base.Remove([]string{"a.COM", " *.b.com ", "never.there"})
	want := Policy{DefaultAction: "deny", Egress: []Rule{{"allow", "10.0.0.0/8"}}}

	if n != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("remove: n=%d %+v, want 2 %+v", n, got, want)
	}

	if _, n := base.Remove([]string{"nope"}); n != 0 {
		t.Fatal("removing an absent target removed something")
	}
}

func TestParseRulesAndTargetsRefuseEmpty(t *testing.T) {
	if _, err := ParseRules([]byte(`[]`)); err == nil {
		t.Error("an empty patch was accepted; upstream refuses it")
	}

	if _, err := ParseTargets([]byte(`[]`)); err == nil {
		t.Error("an empty delete was accepted; upstream refuses it")
	}

	r, err := ParseRules([]byte(`[{"action":"ALLOW","target":"x.com"}]`))
	if err != nil || r[0].Action != "allow" {
		t.Fatalf("ParseRules: %v %+v", err, r)
	}

	if _, err := ParseRules([]byte(`[{"action":"allow","target":"https://x.com/"}]`)); err == nil {
		t.Error("a URL was accepted as a patch target")
	}
}

func TestMode(t *testing.T) {
	for _, c := range []struct {
		p    Policy
		want string
	}{
		{Policy{DefaultAction: "allow"}, "allow_all"},
		{Policy{DefaultAction: "deny"}, "deny_all"},
		{Policy{DefaultAction: "allow", Egress: []Rule{{"deny", "x.com"}}}, "enforcing"},
	} {
		if got := c.p.Mode(); got != c.want {
			t.Errorf("%+v: %s, want %s", c.p, got, c.want)
		}
	}
}

// egress_allow keeps its meaning under the new model: a host permits itself and every
// subdomain, a port is ignored, an IP stays an IP.
func TestFromAllowListKeepsTheSuffixMeaning(t *testing.T) {
	got := FromAllowList([]string{"openai.com", "PyPI.org:443", "10.0.0.5", " ", "openai.com"})
	want := Policy{DefaultAction: "deny", Egress: []Rule{
		{"allow", "openai.com"}, {"allow", "*.openai.com"},
		{"allow", "pypi.org"}, {"allow", "*.pypi.org"},
		{"allow", "10.0.0.5"},
	}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// Live: one Filter, one listener, never restarted - and the next request obeys the new policy.
func TestSetPolicyChangesARunningFilter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	f := NewPolicy(mustPolicy(t, `{"egress":[{"action":"allow","target":"127.0.0.1"}]}`))
	proxy := httptest.NewServer(f)

	defer proxy.Close()

	tr := &http.Transport{Proxy: fixedProxy(t, proxy.URL)}

	get := func() int {
		req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)

		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}

		resp.Body.Close()

		return resp.StatusCode
	}

	if got := get(); got != http.StatusOK {
		t.Fatalf("before: %d", got)
	}

	if err := f.SetPolicy(mustPolicy(t, `{"egress":[{"action":"deny","target":"127.0.0.1"}]}`)); err != nil {
		t.Fatal(err)
	}

	if got := get(); got != http.StatusForbidden {
		t.Fatalf("after locking down, the same running filter answered %d, want 403", got)
	}

	if err := f.SetPolicy(Policy{DefaultAction: "sometimes"}); err == nil {
		t.Fatal("SetPolicy accepted an invalid policy")
	}

	if f.Policy().Egress[0].Action != "deny" {
		t.Fatal("a refused SetPolicy replaced the policy in force")
	}
}

// A CONNECT to an IP literal in a denied range is refused before any socket is opened.
func TestConnectToADeniedCIDRNeverDials(t *testing.T) {
	var accepted atomic.Int64

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			accepted.Add(1)
			c.Close()
		}
	}()

	// Allowed explicitly as a name would be irrelevant: the destination is an address.
	f := NewPolicy(mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"allow","target":"127.0.0.0/8"},{"action":"deny","target":"127.0.0.1/32"}]}`))
	proxy := httptest.NewServer(f)

	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr())

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT to a denied /32 got %d, want 403", resp.StatusCode)
	}

	if accepted.Load() != 0 {
		t.Fatal("the filter opened a connection to a denied address before refusing")
	}
}
