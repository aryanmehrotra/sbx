package cli

import (
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

func tiered(iso provider.Isolation, services ...string) []provider.Unit {
	us := units("x", services...)
	for i := range us {
		us[i].Isolation = iso
	}

	return us
}

// `sbx add x svc` on a gVisor sandbox ran the new service on runc: the flag's default is
// "container", and nothing asked what the sandbox already was. The sandbox's tier is the
// default now, and a different explicit one is refused rather than mixed in.
func TestAddIsolation(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		units     []provider.Unit
		requested provider.Isolation
		explicit  bool
		want      provider.Isolation
		refused   bool
	}{
		{"gvisor sandbox, no flag: joins on gvisor", "docker", tiered(provider.IsolationGVisor, "db"), provider.IsolationContainer, false, provider.IsolationGVisor, false},
		{"kata sandbox, no flag: joins on kata", "docker", tiered(provider.IsolationKata, "db", "web"), provider.IsolationContainer, false, provider.IsolationKata, false},
		{"gvisor sandbox, --isolation container: refused", "docker", tiered(provider.IsolationGVisor, "db"), provider.IsolationContainer, true, "", true},
		{"gvisor sandbox, --isolation gvisor: fine", "docker", tiered(provider.IsolationGVisor, "db"), provider.IsolationGVisor, true, provider.IsolationGVisor, false},
		{"unlabelled (older) sandbox is container", "docker", tiered("", "db"), provider.IsolationContainer, false, provider.IsolationContainer, false},
		{"unlabelled sandbox, --isolation kata: refused", "docker", tiered("", "db"), provider.IsolationKata, true, "", true},
		{"a provider that records no tier is left alone", "firecracker", tiered("", "db"), provider.IsolationFirecracker, true, provider.IsolationFirecracker, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := AddIsolation(c.provider, c.units, c.requested, c.explicit)
			if c.refused {
				if err == nil {
					t.Fatalf("mixing tiers was allowed: got %q", got)
				}

				if !strings.Contains(err.Error(), "--isolation") {
					t.Errorf("the refusal does not say which flag to change: %v", err)
				}

				return
			}

			if err != nil || got != c.want {
				t.Fatalf("AddIsolation = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}
