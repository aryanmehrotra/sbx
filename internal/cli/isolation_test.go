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
		source    string // what asked for the tier: "", "--isolation" or "SBX_ISOLATION"
		want      provider.Isolation
		refused   bool
	}{
		{"gvisor sandbox, no flag: joins on gvisor", "docker", tiered(provider.IsolationGVisor, "db"), provider.IsolationContainer, "", provider.IsolationGVisor, false},
		{"kata sandbox, no flag: joins on kata", "docker", tiered(provider.IsolationKata, "db", "web"), provider.IsolationContainer, "", provider.IsolationKata, false},
		{"gvisor sandbox, --isolation container: refused", "docker", tiered(provider.IsolationGVisor, "db"), provider.IsolationContainer, "--isolation", "", true},
		{"gvisor sandbox, --isolation gvisor: fine", "docker", tiered(provider.IsolationGVisor, "db"), provider.IsolationGVisor, "--isolation", provider.IsolationGVisor, false},
		{"unlabelled (older) sandbox is container", "docker", tiered("", "db"), provider.IsolationContainer, "", provider.IsolationContainer, false},
		{"unlabelled sandbox, --isolation kata: refused", "docker", tiered("", "db"), provider.IsolationKata, "--isolation", "", true},
		{"gvisor sandbox, SBX_ISOLATION=container: refused naming the variable", "docker", tiered(provider.IsolationGVisor, "db"), provider.IsolationContainer, "SBX_ISOLATION", "", true},
		{"a provider that records no tier is left alone", "firecracker", tiered("", "db"), provider.IsolationFirecracker, "--isolation", provider.IsolationFirecracker, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := AddIsolation(c.provider, c.units, c.requested, c.source)
			if c.refused {
				if err == nil {
					t.Fatalf("mixing tiers was allowed: got %q", got)
				}

				if !strings.Contains(err.Error(), c.source) {
					t.Errorf("the refusal does not name %s, which is what asked for the tier: %v", c.source, err)
				}

				// Naming a flag nobody passed sends the reader looking for it on a command line
				// that does not have it.
				if c.source == "SBX_ISOLATION" && strings.Contains(err.Error(), "--isolation "+string(c.requested)+" does not") {
					t.Errorf("the refusal blames a --isolation flag that was never passed: %v", err)
				}

				return
			}

			if err != nil || got != c.want {
				t.Fatalf("AddIsolation = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}
