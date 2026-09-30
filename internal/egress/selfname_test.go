package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// N14, the filter's own names: `GET http://sbx-egress:20999/` was told to list
// "sbx-egress:20999" in egress_allow. sbx-egress is the filter, whose addresses are always
// refused, so that advice could never work. The filter's names are door names: judged without a
// lookup (a refused port costs no DNS query), and answered as a door on any port.
func TestTheFiltersOwnNamesAreDoorsOnAnyPort(t *testing.T) {
	resolved := false

	f := NewPolicy(Policy{DefaultAction: ActionAllow, Egress: []Rule{}})
	f.SelfNames = []string{"sbx-egress", "0123456789ab"}
	f.Refuse = func(netip.Addr) bool { return false }
	f.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		resolved = true
		return []netip.Addr{netip.MustParseAddr("172.25.255.254")}, nil
	}

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	tr := &http.Transport{Proxy: fixedProxy(t, proxy.URL)}

	get := func(u string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, u, nil)

		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()

		b, _ := io.ReadAll(resp.Body)

		return resp.StatusCode, string(b)
	}

	for _, check := range []struct {
		name string
		do   func() (int, string)
	}{
		{"GET sbx-egress:20999", func() (int, string) { return get("http://sbx-egress:20999/") }},
		{"GET SBX-EGRESS.:20998", func() (int, string) { return get("http://SBX-EGRESS.:20998/last") }},
		{"GET the hostname:20999", func() (int, string) { return get("http://0123456789ab:20999/") }},
		{"CONNECT sbx-egress:20999", func() (int, string) { return connectStatus(t, proxy.URL, "sbx-egress:20999") }},
		{"CONNECT the hostname:22", func() (int, string) { return connectStatus(t, proxy.URL, "0123456789ab:22") }},
	} {
		code, body := check.do()
		if code != http.StatusForbidden || strings.Contains(body, "egress_allow") || !strings.Contains(body, "no policy opens") {
			t.Errorf("%s = %d %q, want 403 naming a door and no port grant", check.name, code, body)
		}
	}

	if resolved {
		t.Error("a refused port was resolved: the refusal must come before any lookup")
	}

	// On a carried port the name is refused as a door too, without trusting the resolver.
	if code, body := connectStatus(t, proxy.URL, "sbx-egress:443"); code != http.StatusForbidden || !strings.Contains(body, "no policy opens") {
		t.Errorf("CONNECT sbx-egress:443 = %d %q, want 403 naming a door", code, body)
	}
}

// The compiled container filter knows its own names with no flag: the alias services reach it by,
// localhost, and its hostname (docker sets the container ID).
func TestTheCompiledFilterTreatsItsOwnNamesAsDoors(t *testing.T) {
	bin := buildFilter(t)
	proxyPort, statPort := freePort(t), freePort(t)

	cmd := exec.Command(bin,
		"-policy", `{"defaultAction":"allow","egress":[]}`,
		"-token", "t0ken",
		"-state", filepath.Join(t.TempDir(), "policy.json"),
		"-listen", "127.0.0.1:"+proxyPort,
		"-stat", "127.0.0.1:"+statPort)

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	waitFor(t, "127.0.0.1:"+proxyPort)

	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"sbx-egress:20999", "localhost:20998", host + ":20999"} {
		code, body := connectStatus(t, "http://127.0.0.1:"+proxyPort, target)
		if code != http.StatusForbidden || strings.Contains(body, "egress_allow") || !strings.Contains(body, "no policy opens") {
			t.Errorf("CONNECT %s = %d %q, want 403 naming a door and no port grant", target, code, body)
		}
	}
}
