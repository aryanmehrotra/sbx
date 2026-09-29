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

// N13: a docker network created after a container filter started has a gateway on the VM that the
// filter was never told about. `CONNECT 172.31.0.1:443` reached the VM (502 connection refused:
// the filter dialled it). The daemon now pushes the engine's current gateways over the control
// endpoint; the filter must refuse what it is told, from then on, whatever the policy says.
func TestDoorsRefuseAnAddressPushedAfterStart(t *testing.T) {
	d := fakeDoors(t)
	later := netip.MustParseAddr("172.31.0.1")

	if d.Refuse(later) {
		t.Fatal("the fixture already refuses the late network's gateway; the test proves nothing")
	}

	d.SetPushed([]netip.Prefix{netip.MustParsePrefix("172.31.0.1/32")})

	if !d.Refuse(later) {
		t.Fatal("a gateway pushed after start was not refused")
	}

	// A Refresh re-reads routes and names; it must not drop what the daemon pushed.
	d.Refresh(context.Background())

	if !d.Refuse(later) {
		t.Fatal("a Refresh dropped the pushed set")
	}

	// The pushed set is replaced, never allowed to shrink what the filter found on its own.
	d.SetPushed(nil)

	if d.Refuse(later) {
		t.Error("a replaced pushed set still refused the old entry")
	}

	if !d.Refuse(netip.MustParseAddr("172.17.0.1")) || !d.Refuse(netip.MustParseAddr("192.168.5.2")) {
		t.Error("an empty push opened what the filter's own start list and names refuse")
	}
}

// The push is a control write, so the workload - which can reach this port - must not be able to
// make it. A bad body changes nothing: fail closed on what was refused before.
func TestTheRefusePushNeedsTheTokenAndKeepsTheOldSetOnABadBody(t *testing.T) {
	d := fakeDoors(t)
	ctl := httptest.NewServer(&Control{Filter: NewPolicy(DenyAll()), Doors: d, Token: "tok"})
	defer ctl.Close()

	put := func(token, body string) int {
		req, _ := http.NewRequest(http.MethodPut, ctl.URL+"/refuse", strings.NewReader(body))
		req.Header.Set(TokenHeader, token)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		return resp.StatusCode
	}

	if code := put("", `{"prefixes":["172.31.0.1"]}`); code != http.StatusUnauthorized {
		t.Fatalf("a push without the token got %d, want 401", code)
	}

	if d.Refuse(netip.MustParseAddr("172.31.0.1")) {
		t.Fatal("a push without the token was applied")
	}

	if code := put("tok", `{"prefixes":["172.31.0.1","172.32.0.0/16"]}`); code != http.StatusOK {
		t.Fatalf("a valid push got %d", code)
	}

	for _, a := range []string{"172.31.0.1", "172.32.9.9"} {
		if !d.Refuse(netip.MustParseAddr(a)) {
			t.Errorf("%s was pushed and is not refused", a)
		}
	}

	if code := put("tok", `{"prefixes":["172.33.0.1","not-an-address"]}`); code != http.StatusBadRequest {
		t.Fatalf("a push with a bad entry got %d, want 400", code)
	}

	if !d.Refuse(netip.MustParseAddr("172.31.0.1")) || d.Refuse(netip.MustParseAddr("172.33.0.1")) {
		t.Error("a rejected push changed the refused set")
	}
}

// N15: the activity timestamp is the daemon's, not the workload's. `GET sbx-egress:20998/last`
// answered 200 to the sandbox.
func TestTheLastActivityReadNeedsTheToken(t *testing.T) {
	ctl := httptest.NewServer(&Control{Filter: NewPolicy(DenyAll()), Token: "tok", Last: func() int64 { return 42 }})
	defer ctl.Close()

	get := func(token string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ctl.URL+"/last", nil)
		if token != "" {
			req.Header.Set(TokenHeader, token)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()

		b, _ := io.ReadAll(resp.Body)

		return resp.StatusCode, strings.TrimSpace(string(b))
	}

	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Fatalf("/last without the token got %d, want 401", code)
	}

	if code, body := get("tok"); code != http.StatusOK || body != "42" {
		t.Fatalf("/last with the token = %d %q, want 200 42", code, body)
	}
}

// N14: the port check ran first, so a request to a door on a port the filter does not carry was
// told to list the door in egress_allow - advice that would still be refused. A door says it is a
// door; only an address that would otherwise be allowed gets the port hint.
func TestARefusedAddressIsNeverToldToAddAPortGrant(t *testing.T) {
	d := fakeDoors(t)

	f := NewPolicy(Policy{DefaultAction: ActionAllow, Egress: []Rule{}})
	f.Refuse = d.Refuse
	f.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "example.com" {
			return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
		}

		return d.Resolve(ctx, host)
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
		{"GET host.lima.internal:28777", func() (int, string) { return get("http://host.lima.internal:28777/") }},
		{"GET 172.17.0.1:28777", func() (int, string) { return get("http://172.17.0.1:28777/") }},
		{"CONNECT host.lima.internal:28777", func() (int, string) { return connectStatus(t, proxy.URL, "host.lima.internal:28777") }},
		{"CONNECT 192.168.5.2:28777", func() (int, string) { return connectStatus(t, proxy.URL, "192.168.5.2:28777") }},
	} {
		code, body := check.do()
		if code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", check.name, code)
		}

		if strings.Contains(body, "egress_allow") || !strings.Contains(body, "no policy opens") {
			t.Errorf("%s: the refusal suggests a grant that would still be refused, or does not say "+
				"it is a door: %q", check.name, body)
		}
	}

	// An address that would be allowed still gets the hint that opens it.
	code, body := connectStatus(t, proxy.URL, "example.com:8443")
	if code != http.StatusForbidden || !strings.Contains(body, `"example.com:8443" in the service's egress_allow`) {
		t.Errorf("an allowed host on an uncarried port lost its hint: %d %q", code, body)
	}

	// A host the policy denies is told that, not handed a port grant for a host it cannot reach.
	if err := f.SetPolicy(DenyAll()); err != nil {
		t.Fatal(err)
	}

	code, body = connectStatus(t, proxy.URL, "example.com:8443")
	if code != http.StatusForbidden || !strings.Contains(body, "egress not allowed: example.com") ||
		strings.Contains(body, "egress_allow") {
		t.Errorf("a denied host on an uncarried port: %d %q", code, body)
	}
}

// The compiled container filter: /last is token-gated, and a refuse set pushed over the control
// endpoint is enforced by the running process on the next request and survives a restart.
func TestTheCompiledFilterTakesARefusePushAndGatesLast(t *testing.T) {
	bin := buildFilter(t)
	dir := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "a network created after the filter")
	}))
	defer upstream.Close()

	target := strings.TrimPrefix(upstream.URL, "http://")

	start := func() (proxy, stat string, stop func()) {
		proxyPort, statPort := freePort(t), freePort(t)

		cmd := exec.Command(bin,
			"-policy", `{"defaultAction":"allow","egress":[{"action":"allow","target":"127.0.0.0/8"}]}`,
			"-ports", target,
			"-token", "t0ken",
			"-state", filepath.Join(dir, "policy.json"),
			"-listen", "127.0.0.1:"+proxyPort,
			"-stat", "127.0.0.1:"+statPort)

		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		waitFor(t, "127.0.0.1:"+proxyPort)
		waitFor(t, "127.0.0.1:"+statPort)

		return "http://127.0.0.1:" + proxyPort, "http://127.0.0.1:" + statPort,
			func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
	}

	proxy, stat, stop := start()

	do := func(method, u, token, body string) (int, string) {
		req, _ := http.NewRequest(method, u, strings.NewReader(body))
		if token != "" {
			req.Header.Set(TokenHeader, token)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()

		b, _ := io.ReadAll(resp.Body)

		return resp.StatusCode, string(b)
	}

	if code, body := do(http.MethodGet, stat+"/last", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("/last without the token = %d %q, want 401: the workload reads the daemon's idle signal", code, body)
	}

	if code, body := do(http.MethodGet, stat+"/last", "t0ken", ""); code != http.StatusOK || strings.TrimSpace(body) == "" {
		t.Fatalf("/last with the token = %d %q", code, body)
	}

	if code, _ := connectStatus(t, proxy, target); code != http.StatusOK {
		t.Fatalf("before the push the fixture was not reachable (%d); the test proves nothing", code)
	}

	if code, body := do(http.MethodPut, stat+"/refuse", "", `{"prefixes":["127.0.0.1"]}`); code != http.StatusUnauthorized {
		t.Fatalf("a refuse push without the token = %d %q, want 401", code, body)
	}

	if code, body := do(http.MethodPut, stat+"/refuse", "t0ken", `{"prefixes":["127.0.0.1"]}`); code != http.StatusOK {
		t.Fatalf("the refuse push = %d %q", code, body)
	}

	if code, body := connectStatus(t, proxy, target); code != http.StatusForbidden || !strings.Contains(body, "no policy opens") {
		t.Fatalf("after the push CONNECT = %d %q, want 403 naming a door", code, body)
	}

	// Restarted - a reboot, a docker restart - it comes back refusing it, before the daemon's
	// next push.
	stop()

	proxy, _, stop = start()
	defer stop()

	if code, body := connectStatus(t, proxy, target); code != http.StatusForbidden {
		t.Fatalf("after a restart the pushed set was lost: CONNECT = %d %q", code, body)
	}

	if _, err := os.Stat(filepath.Join(dir, "refuse.json")); err != nil {
		t.Errorf("the pushed set was not saved beside the policy: %v", err)
	}
}
