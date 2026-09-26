package daemon

import (
	"net/netip"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A microVM filter under egress: "allow" refuses private ranges - docker containers, the LAN,
// the VPC - whatever the sandbox's policy says, and only the operator's --vm-egress-allow opens one.
func TestAMicroVMFilterRefusesPrivateRangesUnlessTheOperatorWidens(t *testing.T) {
	open, _ := egress.Policy{DefaultAction: egress.ActionAllow, Egress: []egress.Rule{
		{Action: egress.ActionAllow, Target: "0.0.0.0/0"}}}.Normalize()
	body, _ := open.MarshalJSON()

	host := func(widen []netip.Prefix) *egress.Filter {
		d := &daemon{egressDir: t.TempDir(), egress: map[string]*egressProxy{}, egressPort: freePort(t),
			vmWiden: widen}
		d.reconcileEgress([]provider.Unit{{Sandbox: "vm", Service: "cache", EgressGateway: "127.0.0.1",
			EgressBridge: "sbxfc0", EgressPolicy: string(body)}})

		px := d.egress["127.0.0.1"]
		if px == nil {
			t.Fatal("no filter hosted")
		}

		t.Cleanup(func() { _ = px.ln.Close() })

		return px.filter
	}

	f := host(nil)
	for _, a := range []string{"172.17.0.3", "10.20.0.5", "192.168.1.1", "100.64.0.1", "fd00::1"} {
		if f.Permits(a) {
			t.Errorf("a policy allowing 0.0.0.0/0 opened private %s", a)
		}
	}

	if !f.Permits("93.184.215.14") {
		t.Fatal("the internet was closed")
	}

	w, err := parseCIDRs(" 10.20.0.0/16 ,")
	if err != nil {
		t.Fatal(err)
	}

	f = host(w)
	if !f.Permits("10.20.0.5") || f.Permits("10.21.0.5") || f.Permits("10.231.0.1") {
		t.Fatal("--vm-egress-allow did not open exactly its range, and never the plan")
	}

	if _, err := parseCIDRs("10.20.0.0"); err == nil {
		t.Fatal("a bare address was taken as a CIDR")
	}
}
