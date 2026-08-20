package spec

import "testing"

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
		{"allow", false}, // allow is the absence of the field, not a value
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
