package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
)

// A container's /proc/net/route on colima: eth0 is the sandbox's no-NAT bridge, eth1 the default
// bridge that carries the default route. Gateways are little-endian hex.
const colimaRoutes = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth1	00000000	010011AC	0003	0	0	0	00000000	0	0	0
eth0	000013AC	00000000	0001	0	0	0	0000FFFF	0	0	0
eth1	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
`

func TestRouteGatewaysReadsTheDefaultRoutesNextHop(t *testing.T) {
	got := RouteGateways(colimaRoutes)
	if fmt.Sprint(got) != "[172.17.0.1]" {
		t.Fatalf("gateways = %v, want [172.17.0.1]", got)
	}
}

// fakeDoors is colima as seen from inside the filter container.
func fakeDoors(t *testing.T) *Doors {
	t.Helper()

	static, err := ParsePrefixes("172.19.0.1,172.20.0.1,172.17.0.0/16")
	if err != nil {
		t.Fatal(err)
	}

	d := &Doors{
		Static: static,
		Routes: func() (string, error) { return colimaRoutes, nil },
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "host.lima.internal", "host.docker.internal":
				return []netip.Addr{netip.MustParseAddr("192.168.5.2")}, nil
			default:
				return nil, errors.New("no such host") // gateway.docker.internal is not on colima
			}
		},
	}
	d.Refresh(context.Background())

	return d
}

// The container filter is the one door out of a sandbox, and on a VM-backed docker everything in
// the VM - and, through it, the Mac's loopback - is one hop from it. Under a default of allow,
// CONNECT host.lima.internal:<port> reached a listener bound only to the Mac's 127.0.0.1, and
// CONNECT 172.17.0.1:22 the VM's sshd. None of those is anything a policy may open.
func TestTheContainerFilterRefusesTheMachinesBehindIt(t *testing.T) {
	d := fakeDoors(t)

	f := NewPolicy(Policy{DefaultAction: ActionAllow, Egress: []Rule{
		{Action: ActionAllow, Target: "0.0.0.0/0"}, // the widest thing a caller could write
		{Action: ActionAllow, Target: "host.lima.internal"},
	}})
	f.Refuse = d.Refuse
	f.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "example.com" {
			return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
		}

		return d.Resolve(ctx, host)
	}

	for _, host := range []string{
		"172.17.0.1",   // the default route's next hop: the VM
		"172.19.0.1",   // the sandbox bridge's gateway, given by the provider
		"172.20.0.1",   // another docker network's gateway: also the VM
		"172.17.0.5",   // another container on the default bridge
		"192.168.5.2",  // host.lima.internal: the Mac
		"192.168.5.15", // the VM's own address on its link to the Mac
	} {
		if f.Permits(host) {
			t.Errorf("%s was permitted: it is the machine behind the filter", host)
		}
	}

	for _, name := range []string{"host.lima.internal", "host.docker.internal"} {
		if _, err := f.admit(context.Background(), name); err == nil {
			t.Errorf("%s was admitted", name)
		}
	}

	if _, err := f.admit(context.Background(), "example.com"); err != nil {
		t.Errorf("the internet was closed: %v", err)
	}

	if f.Permits("172.19.0.3") != true {
		t.Error("a sibling service on the sandbox's own bridge was refused; only its gateway is the VM")
	}
}

// A filter that has not yet looked refuses rather than guessing.
func TestDoorsRefuseEverythingBeforeTheFirstRefresh(t *testing.T) {
	if !(&Doors{}).Refuse(netip.MustParseAddr("93.184.215.14")) {
		t.Fatal("an unrefreshed door set let an address through")
	}
}

// The compiled container filter takes the provider's list on -refuse, and no allow rule opens it.
// This is the program the container runs, so it is the one that has to refuse.
func TestTheCompiledFilterRefusesWhatItIsToldToRefuse(t *testing.T) {
	bin := buildFilter(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "the machine behind the filter")
	}))
	defer upstream.Close()

	proxyPort, statPort := freePort(t), freePort(t)

	cmd := exec.Command(bin,
		"-policy", `{"defaultAction":"allow","egress":[{"action":"allow","target":"127.0.0.0/8"}]}`,
		"-ports", strings.TrimPrefix(upstream.URL, "http://"),
		"-refuse", "127.0.0.1",
		"-token", "t0ken",
		"-state", t.TempDir()+"/policy.json",
		"-listen", "127.0.0.1:"+proxyPort,
		"-stat", "127.0.0.1:"+statPort)

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	defer func() { _ = cmd.Process.Kill() }()

	waitFor(t, "127.0.0.1:"+proxyPort)

	code, body := connectStatus(t, "http://127.0.0.1:"+proxyPort, strings.TrimPrefix(upstream.URL, "http://"))
	if code != http.StatusForbidden {
		t.Fatalf("CONNECT to a refused address under an allow rule for it = %d (%s), want 403", code, body)
	}
}
