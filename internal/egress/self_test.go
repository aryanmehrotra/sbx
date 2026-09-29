package egress

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The filter's own addresses. `CONNECT sbx-egress:443` from a sandbox made the filter dial itself
// at 172.25.255.254:443 (502 connection refused): harmless while nothing listens there, but the
// filter's control port listens on the same addresses, and a door the filter opens onto itself is
// a door. Every interface address is refused - the address, not its subnet, because the sandbox's
// own services share that subnet.
func selfDoors(t *testing.T) *Doors {
	t.Helper()

	d := fakeDoors(t)
	d.Interfaces = func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("172.25.255.254"), Mask: net.CIDRMask(16, 32)},
			&net.IPNet{IP: net.ParseIP("172.17.0.9"), Mask: net.CIDRMask(16, 32)},
			&net.IPNet{IP: net.ParseIP("fd00::9"), Mask: net.CIDRMask(64, 128)},
		}, nil
	}
	d.Refresh(context.Background())

	return d
}

func TestDoorsRefuseTheFiltersOwnAddresses(t *testing.T) {
	d := selfDoors(t)

	for _, a := range []string{"127.0.0.1", "172.25.255.254", "fd00::9"} {
		if !d.Refuse(netip.MustParseAddr(a)) {
			t.Errorf("the filter's own address %s is not refused", a)
		}
	}

	if d.Refuse(netip.MustParseAddr("172.25.0.3")) {
		t.Error("a sibling service on the sandbox's own subnet was refused; only the filter's address is")
	}
}

func TestConnectToTheFilterItselfIsRefusedAsADoor(t *testing.T) {
	d := selfDoors(t)

	f := NewPolicy(Policy{DefaultAction: ActionAllow, Egress: []Rule{}})
	f.Refuse = d.Refuse
	f.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "sbx-egress" {
			return []netip.Addr{netip.MustParseAddr("172.25.255.254")}, nil
		}

		return d.Resolve(ctx, host)
	}

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	for _, target := range []string{"sbx-egress:443", "172.25.255.254:443"} {
		code, body := connectStatus(t, proxy.URL, target)
		if code != http.StatusForbidden || !strings.Contains(body, "no policy opens") {
			t.Errorf("CONNECT %s = %d %q, want 403 naming a door", target, code, body)
		}
	}
}

// The compiled container filter refuses a CONNECT to its own listener - here its own proxy port on
// loopback, which it would otherwise tunnel to under an allow rule and a port grant for it.
func TestTheCompiledFilterNeverDialsItself(t *testing.T) {
	bin := buildFilter(t)
	proxyPort, statPort := freePort(t), freePort(t)
	self := "127.0.0.1:" + proxyPort

	cmd := exec.Command(bin,
		"-policy", `{"defaultAction":"allow","egress":[{"action":"allow","target":"127.0.0.0/8"}]}`,
		"-ports", self,
		"-token", "t0ken",
		"-state", filepath.Join(t.TempDir(), "policy.json"),
		"-listen", self,
		"-stat", "127.0.0.1:"+statPort)

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	waitFor(t, self)

	code, body := connectStatus(t, "http://"+self, self)
	if code != http.StatusForbidden || !strings.Contains(body, "no policy opens") {
		t.Fatalf("CONNECT to the filter's own address = %d %q, want 403 naming a door", code, body)
	}
}
