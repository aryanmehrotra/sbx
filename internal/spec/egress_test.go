package spec

import (
	"testing"

	"github.com/aryanmehrotra/sbx/internal/egress"
)

// A typo in a security control must fail rather than leave egress open. "den" is not
// "deny", and the difference is a sandbox that can reach the internet while its spec says
// it cannot.
func TestEgressValidation(t *testing.T) {
	for _, c := range []struct {
		value string
		ok    bool
	}{
		{"", true}, // unset is what every existing spec already has
		{"deny", true},
		{"den", false},
		{"Deny", false},  // case matters; a silent lowercase hides the next typo
		{"allow", true},  // open, but through the filter, so it can be narrowed live
		{"Allow", false}, // the same rule as Deny
		{"none", false},
	} {
		err := Service{Image: "x", Ports: []int{1}, Egress: c.value}.validate("svc")

		if c.ok && err != nil {
			t.Errorf("egress %q: unexpected error: %v", c.value, err)
		}

		if !c.ok && err == nil {
			t.Errorf("egress %q was accepted and would have left egress open", c.value)
		}
	}
}

// The error has to name the service and the only valid value, or it sends someone to the
// source to find out what to write.
func TestEgressErrorIsActionable(t *testing.T) {
	err := Service{Image: "x", Ports: []int{1}, Egress: "den"}.validate("db")
	if err == nil {
		t.Fatal("expected an error")
	}

	for _, want := range []string{"db", "den", "deny"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}

// egress_allow is enforced by a filtering proxy; a spec that pairs it with egress "deny" would
// deny the allowed hosts too, and a blank host is a hole. Both must be caught at load time.
func TestEgressAllowValidation(t *testing.T) {
	ok := Service{Image: "x", Ports: []int{1}, EgressAllow: []string{"api.openai.com", "pypi.org"}}
	if err := ok.validate("svc"); err != nil {
		t.Errorf("a plain allow-list should be valid: %v", err)
	}

	both := Service{Image: "x", Ports: []int{1}, Egress: "deny", EgressAllow: []string{"a.com"}}
	if both.validate("svc") == nil {
		t.Error("egress deny + egress_allow together was accepted; it denies the allowed hosts")
	}

	blank := Service{Image: "x", Ports: []int{1}, EgressAllow: []string{"a.com", "  "}}
	if blank.validate("svc") == nil {
		t.Error("a blank egress_allow host was accepted; it is a hole in the list")
	}
}

func TestIdleValidation(t *testing.T) {
	for _, ok := range []string{"", "never", "0", "30m", "2h"} {
		if err := (Service{Image: "x", Ports: []int{1}, Idle: ok}).validate("s"); err != nil {
			t.Errorf("idle %q should be valid: %v", ok, err)
		}
	}

	if (Service{Image: "x", Ports: []int{1}, Idle: "soon"}).validate("s") == nil {
		t.Error("idle \"soon\" was accepted; it is neither never, 0, nor a duration")
	}
}

// egress, egress_allow and egress_policy each state the whole answer; two of them together is a
// contradiction, and resolving it by precedence would silently drop half of a security control.
func TestEgressFieldsThatContradictAreRefused(t *testing.T) {
	pol := &egress.Policy{DefaultAction: "allow", Egress: []egress.Rule{{Action: "deny", Target: "1.1.1.0/24"}}}

	for _, c := range []struct {
		name string
		svc  Service
		ok   bool
	}{
		{"policy alone", Service{EgressPolicy: pol}, true},
		{"allow alone", Service{Egress: "allow"}, true},
		{"deny + list", Service{Egress: "deny", EgressAllow: []string{"a.com"}}, false},
		{"allow + list", Service{Egress: "allow", EgressAllow: []string{"a.com"}}, false},
		{"allow + policy", Service{Egress: "allow", EgressPolicy: pol}, false},
		{"deny + policy", Service{Egress: "deny", EgressPolicy: pol}, false},
		{"list + policy", Service{EgressAllow: []string{"a.com"}, EgressPolicy: pol}, false},
		{"bad policy", Service{EgressPolicy: &egress.Policy{Egress: []egress.Rule{{Action: "deny", Target: "https://x.com"}}}}, false},
	} {
		c.svc.Image, c.svc.Ports = "x", []int{1}

		err := c.svc.validate("svc")
		if c.ok != (err == nil) {
			t.Errorf("%s: ok=%v, err=%v", c.name, c.ok, err)
		}
	}
}

func TestDeclaredPolicy(t *testing.T) {
	if p := (Service{Egress: "allow"}).DeclaredPolicy(); p.Mode() != "allow_all" {
		t.Errorf("egress allow declares %+v, want allow_all", p)
	}

	if p := (Service{EgressAllow: []string{"openai.com"}}).DeclaredPolicy(); len(p.Egress) != 2 || p.DefaultAction != "deny" {
		t.Errorf("an allow-list declares %+v", p)
	}

	if (Service{}).Filtered() || (Service{Egress: "deny"}).Filtered() {
		t.Error("a service with no filter reports one")
	}
}

// Services of a sandbox share one filter, so two different policies in one spec are refused
// rather than enforced as a mixture nobody wrote.
func TestTwoPoliciesInOneSandboxMustAgree(t *testing.T) {
	spec := func(a, b string) []byte {
		return []byte(`{"version":1,"services":{` +
			`"a":{"image":"x","ports":[1],` + a + `},` +
			`"b":{"image":"x","ports":[2],` + b + `}}}`)
	}

	same := `"egress_policy":{"defaultAction":"allow","egress":[{"action":"deny","target":"x.com"}]}`

	if _, err := ParseSpec(spec(same, same), "s.json"); err != nil {
		t.Errorf("two services with one policy were refused: %v", err)
	}

	if _, err := ParseSpec(spec(same, `"egress":"allow"`), "s.json"); err == nil {
		t.Error("two services with different policies were accepted")
	}

	if _, err := ParseSpec(spec(same, `"egress_allow":["a.com"]`), "s.json"); err == nil {
		t.Error("a policy and an allow-list on one filter were accepted")
	}

	if _, err := ParseSpec(spec(`"egress_allow":["a.com"]`, `"egress_allow":["b.com"]`), "s.json"); err != nil {
		t.Errorf("two allow-lists, whose union has always been the meaning, were refused: %v", err)
	}
}
