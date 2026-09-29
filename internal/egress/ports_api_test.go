package egress

import (
	"fmt"
	"testing"
)

func TestPortGrantsFromAllowList(t *testing.T) {
	got := PortGrantsFromAllowList([]string{"GitHub.com:22", "pypi.org", "[2001:db8::1]:5432", "10.0.0.0/8:5432", "x.org:443"})

	want := []PortGrant{{Target: "github.com", Port: 22}, {Target: "2001:db8::1", Port: 5432},
		{Target: "10.0.0.0/8", Port: 5432}}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("grants = %v, want %v", got, want)
	}

	if FormatPortGrants(got) != "github.com:22,[2001:db8::1]:5432,10.0.0.0/8:5432" {
		t.Errorf("FormatPortGrants = %q", FormatPortGrants(got))
	}

	back, err := ParsePortGrants(FormatPortGrants(got))
	if err != nil || fmt.Sprint(back) != fmt.Sprint(want) {
		t.Errorf("round trip = %v, %v", back, err)
	}
}

func TestCheckAllowEntryRefusesABadPort(t *testing.T) {
	for _, bad := range []string{"github.com:ssh", "github.com:0", "github.com:70000", "github.com:"} {
		if err := CheckAllowEntry(bad); err == nil {
			t.Errorf("CheckAllowEntry(%q) = nil, want an error", bad)
		}
	}

	for _, ok := range []string{"github.com", "github.com:22", "10.0.0.5", "[::1]:8080", "2001:db8::1", "10.0.0.0/8"} {
		if err := CheckAllowEntry(ok); err != nil {
			t.Errorf("CheckAllowEntry(%q) = %v", ok, err)
		}
	}
}
