package daemon

import (
	"fmt"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A filter the daemon hosts carries the ports its sandbox's egress_allow entries name, read from
// the units' labels, and picks up a changed list without being restarted: a sandbox recreated
// under the same name keeps its gateway, and so this filter.
func TestAHostedFilterCarriesTheDeclaredPortGrants(t *testing.T) {
	d := &daemon{egressDir: t.TempDir(), egress: map[string]*egressProxy{}, egressPort: freePort(t)}

	unit := func(allow ...string) provider.Unit {
		return provider.Unit{Sandbox: "sb", Service: "app", EgressGateway: "127.0.0.1", EgressAllow: allow}
	}

	d.reconcileEgress([]provider.Unit{unit("github.com:22", "pypi.org")})

	px := d.egress["127.0.0.1"]
	if px == nil {
		t.Fatal("no filter hosted")
	}

	t.Cleanup(func() { _ = px.ln.Close() })

	if got := fmt.Sprint(px.filter.Ports()); got != fmt.Sprint([]egress.PortGrant{{Target: "github.com", Port: 22}}) {
		t.Fatalf("hosted grants = %s, want github.com:22", got)
	}

	d.reconcileEgress([]provider.Unit{unit("github.com", "pypi.org")})

	if got := px.filter.Ports(); len(got) != 0 {
		t.Fatalf("a removed port kept being carried: %v", got)
	}
}
