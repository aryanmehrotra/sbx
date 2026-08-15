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
