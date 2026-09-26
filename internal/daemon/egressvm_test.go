package daemon

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

func connectStatus(t *testing.T, proxy, target string) int {
	t.Helper()

	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)

	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode
}

// A microVM's filter is hosted like any other the daemon can bind, and on top of that refuses
// the host it runs on - the gateway's other ports, other sandboxes' guests - even under an open
// default. A docker bridge's filter is left exactly as it was.
func TestAMicroVMFilterIsHostedAndRefusesTheHostBehindIt(t *testing.T) {
	open, _ := egress.Policy{DefaultAction: egress.ActionAllow, Egress: []egress.Rule{}}.Normalize()
	body, _ := open.MarshalJSON()

	vmUnit := provider.Unit{Sandbox: "vm", Service: "cache", EgressGateway: "127.0.0.1",
		EgressBridge: "sbxfc0", EgressPolicy: string(body)}

	port := freePort(t)
	d := &daemon{egressDir: t.TempDir(), egress: map[string]*egressProxy{}, egressPort: port,
		provider: &filterProvider{f: provider.EgressFilter{Sandbox: "vm", Gateway: "127.0.0.1",
			Services: []string{"cache"}, Declared: open}}}

	d.reconcileEgress([]provider.Unit{vmUnit})

	px := d.egress["127.0.0.1"]
	if px == nil {
		t.Fatal("no filter hosted for the microVM's gateway")
	}

	t.Cleanup(func() { _ = px.ln.Close() })

	if px.filter.Refuse == nil || px.filter.Permits("10.231.0.1") || px.filter.Permits("10.231.9.2") {
		t.Fatal("the microVM's filter would carry a guest onto its own host or a neighbour")
	}

	if !px.filter.Permits("93.184.215.14") {
		t.Fatal("an open default no longer opens the internet")
	}

	proxy := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	if got := connectStatus(t, proxy, "10.231.0.1:22"); got != http.StatusForbidden {
		t.Fatalf("CONNECT to the gateway's sshd through the filter = %d, want 403", got)
	}

	// A docker bridge's filter is not given the refusal: nothing about docker changed.
	dock := vmUnit
	dock.Sandbox, dock.EgressBridge = "ctr", ""

	d2 := &daemon{egressDir: t.TempDir(), egress: map[string]*egressProxy{}, egressPort: freePort(t)}
	d2.reconcileEgress([]provider.Unit{dock})

	if px2 := d2.egress["127.0.0.1"]; px2 == nil || px2.filter.Refuse != nil {
		t.Fatalf("docker's hosted filter changed: %+v", px2)
	} else {
		_ = px2.ln.Close()
	}

	// A live change through the daemon's own API reaches the hosted VM filter in place.
	if _, err := d.Egress().PatchPolicy(context.Background(), "vm", "",
		[]egress.Rule{{Action: egress.ActionDeny, Target: "93.184.215.0/24"}}); err != nil {
		t.Fatal(err)
	}

	if px.filter.Permits("93.184.215.14") || d.egress["127.0.0.1"] != px {
		t.Fatal("the PATCH did not reach the running filter in place")
	}
}
