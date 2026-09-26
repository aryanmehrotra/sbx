package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

var vmPlan = netip.MustParsePrefix("10.231.0.0/16")

func fakeAddrs(ips ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		var out []net.Addr
		for _, s := range ips {
			out = append(out, &net.IPNet{IP: net.ParseIP(s), Mask: net.CIDRMask(24, 32)})
		}

		return out, nil
	}
}

func TestOnThisHostIsTheRangesAndEveryInterfaceAddress(t *testing.T) {
	on := onThisHost(fakeAddrs("192.168.5.15", "172.17.0.1"), []netip.Prefix{vmPlan})

	for s, want := range map[string]bool{
		"10.231.5.1":        true, // the gateway the filter itself is on
		"10.231.7.2":        true, // another sandbox's guest
		"192.168.5.15":      true, // the host's own LAN address
		"::ffff:172.17.0.1": true, // a docker bridge of this host, as a mapped address
		"192.168.5.16":      false,
		"93.184.215.14":     false,
	} {
		if got := on(netip.MustParseAddr(s)); got != want {
			t.Errorf("%s: %v, want %v", s, got, want)
		}
	}

	// A host whose interfaces cannot be read is treated as one where everything is local.
	broken := onThisHost(func() ([]net.Addr, error) { return nil, errors.New("netlink") }, nil)
	if !broken(netip.MustParseAddr("93.184.215.14")) {
		t.Fatal("an unreadable interface list opened the door")
	}
}

// Under default-allow, a filter on the host would otherwise carry a guest to the host's own
// services and to other sandboxes - neither reachable from the guest directly. An explicit allow
// rule for the address still opens it, as it does for loopback.
func TestAFilterOnTheHostRefusesTheHostUnlessARuleNamesIt(t *testing.T) {
	open := Policy{DefaultAction: ActionAllow, Egress: []Rule{}}

	f := NewPolicy(open)
	f.Refuse = onThisHost(fakeAddrs("192.168.5.15"), []netip.Prefix{vmPlan})
	f.Resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		return map[string][]netip.Addr{
			"gw.example":     {netip.MustParseAddr("10.231.0.1")},
			"public.example": {netip.MustParseAddr("93.184.215.14")},
		}[host], nil
	}

	for host, want := range map[string]bool{
		"10.231.0.1":     false,
		"10.231.3.2":     false,
		"192.168.5.15":   false,
		"93.184.215.14":  true,
		"public.example": true,
	} {
		if got := f.Permits(host); got != want {
			t.Errorf("Permits(%s) = %v, want %v", host, got, want)
		}
	}

	// A name that resolves onto the host is refused too: the rebinding guard covers it.
	if _, err := f.admit(context.Background(), "gw.example"); err == nil {
		t.Error("a name resolving to the gateway was admitted")
	}

	if _, err := f.admit(context.Background(), "public.example"); err != nil {
		t.Errorf("public name refused: %v", err)
	}

	named, _ := Policy{DefaultAction: ActionAllow, Egress: []Rule{{Action: ActionAllow, Target: "10.231.0.1"}}}.Normalize()
	if err := f.SetPolicy(named); err != nil {
		t.Fatal(err)
	}

	if !f.Permits("10.231.0.1") || f.Permits("10.231.0.2") {
		t.Error("an allow rule naming the address did not open exactly it")
	}

	// And end to end: the CONNECT gets 403, and the listener on the "host" is never dialled.
	if err := f.SetPolicy(open); err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprint(conn, "CONNECT 10.231.0.1:22 HTTP/1.1\r\nHost: 10.231.0.1:22\r\n\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT to the gateway = %d, want 403", resp.StatusCode)
	}
}
