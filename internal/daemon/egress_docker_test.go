package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// TestLiveEgressPolicyOnARealSandbox is the feature run rather than described, on a real docker:
// a sandbox created with OpenSandbox's default-allow-with-exceptions policy, its policy changed
// while it runs, and a client inside it that ignores the proxy and dials an address directly.
//
// Three claims, each checked from inside the container:
//
//   - a CIDR deny holds for a direct dial as well as through the proxy - the direct dial has no
//     route at all, because the bridge has no NAT, so the proxy is the only way out;
//   - a policy changed through EgressControl is in force on the next request;
//   - nothing was restarted to get there: the service and its filter keep their container IDs
//     and start times.
//
// It creates its own uniquely named sandbox on a slot far above the daemon's range, and removes
// exactly what it created. Where the bridge gateway is bindable (native Linux) the filter is a
// listener in a daemon this test hosts; elsewhere (colima, Docker Desktop) it is the container
// filter - the test follows whichever the provider chose.
func TestLiveEgressPolicyOnARealSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real containers and may build the filter image; not for -short")
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	p, err := provider.For("docker", "", "")
	if err != nil {
		t.Skipf("no docker daemon: %v", err)
	}

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	sandbox := "sbxt-egress-" + hex.EncodeToString(suffix)
	service := "svc"
	ref := "sbx-" + sandbox + "-" + service

	// Cleanup by name, not only through Remove: Remove finds a sandbox by its units, and a
	// create that failed half way has a network and a filter but maybe no unit. Every name here
	// carries this test's random suffix, so nothing else can match.
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()

		_ = p.Remove(c, sandbox)

		for _, args := range [][]string{
			{"rm", "-f", ref},
			{"rm", "-f", "sbx-egressfilter-" + sandbox},
			{"network", "rm", "sbx-noegress-" + sandbox},
		} {
			_ = exec.CommandContext(c, "docker", args...).Run()
		}
	})

	slot, public := freeSlot(t)

	svc := spec.Service{
		Image: "redis:7-alpine", // long-running by default, and its busybox has wget and nc
		Ports: []int{6379},
		EgressPolicy: &egress.Policy{DefaultAction: egress.ActionAllow, Egress: []egress.Rule{
			{Action: egress.ActionDeny, Target: "1.1.1.0/24"},
		}},
	}

	if pf, ok := p.(provider.EgressPreflighter); ok {
		if err := pf.EgressPreflight(ctx, sandbox); err != nil {
			t.Fatalf("preflight: %v", err)
		}
	}

	if err := p.Create(ctx, sandbox, slot, 0, service, svc,
		[]provider.Endpoint{{Host: "127.0.0.1", Port: public}}, t.TempDir(), provider.IsolationContainer); err != nil {
		t.Fatalf("create: %v", err)
	}

	units, err := p.List(ctx, sandbox)
	if err != nil || len(units) != 1 {
		t.Fatalf("list: %v %+v", err, units)
	}

	// Host the filter the way `sbx serve` would, if this machine is the one that hosts it.
	d := &daemon{provider: p, egressDir: t.TempDir(), egress: map[string]*egressProxy{}}
	d.reconcileEgress(units)

	t.Cleanup(func() {
		for _, px := range d.egress {
			_ = px.ln.Close()
		}
	})

	filterRef := "sbx-egressfilter-" + sandbox
	containerFilter := units[0].EgressStat != ""

	before := identity(t, ctx, ref)

	var filterBefore string
	if containerFilter {
		filterBefore = identity(t, ctx, filterRef)
	}

	// 1. A direct dial, ignoring the proxy, to an address in the denied range: no route.
	direct := execIn(t, ctx, ref, `unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY; `+
		`if nc -w 4 1.1.1.1 80 </dev/null; then echo DIRECT-OK; else echo DIRECT-FAILED; fi`)
	if !strings.Contains(direct, "DIRECT-FAILED") {
		t.Fatalf("a client that ignored the proxy reached 1.1.1.1 directly - the bridge routes "+
			"out, and no CIDR rule can be enforced on it:\n%s", direct)
	}

	// And an address the policy does NOT deny is equally unreachable directly: the failure
	// above is the missing route, not luck with one address.
	if out := execIn(t, ctx, ref, `unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY; `+
		`if nc -w 4 8.8.8.8 53 </dev/null; then echo DIRECT-OK; else echo DIRECT-FAILED; fi`); !strings.Contains(out, "DIRECT-FAILED") {
		t.Fatalf("a direct dial to an address no rule mentions got out, so the bridge has a "+
			"route and the proxy is not the only door:\n%s", out)
	}

	// 2. Through the proxy, the denied range is refused by the filter, and one outside it is not.
	if out := fetch(t, ctx, ref, "https://1.1.1.1/"); !strings.Contains(out, "403") {
		t.Fatalf("CONNECT to a denied CIDR through the filter was not refused:\n%s", out)
	}

	if out := fetch(t, ctx, ref, "http://example.com/"); strings.Contains(out, "403") {
		t.Fatalf("default allow refused a host no rule mentions:\n%s", out)
	}

	// 3. Live: deny example.com on the running sandbox, and the very next request is refused.
	st, err := d.Egress().PatchPolicy(ctx, sandbox, service, []egress.Rule{
		{Action: egress.ActionDeny, Target: "example.com"},
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	if out := fetch(t, ctx, ref, "http://example.com/"); !strings.Contains(out, "403") {
		t.Fatalf("a live deny did not take effect on the running sandbox (policy %+v):\n%s", st.Policy, out)
	}

	got, err := d.Egress().GetPolicy(ctx, sandbox, "")
	if err != nil || got.Policy.Egress[0].Target != "example.com" {
		t.Fatalf("GET after the patch: %+v %v", got.Policy, err)
	}

	// 4. Nothing restarted to get there.
	if after := identity(t, ctx, ref); after != before {
		t.Fatalf("the service was restarted to change its policy: %s -> %s", before, after)
	}

	if containerFilter {
		if after := identity(t, ctx, filterRef); after != filterBefore {
			t.Fatalf("the filter container was restarted to change the policy: %s -> %s", filterBefore, after)
		}
	}

	t.Logf("container filter=%v: direct dials had no route, the denied CIDR got 403 through the "+
		"proxy, and a live deny took effect with both containers untouched (%s)", containerFilter, before)
}

// freeSlot picks a slot whose public and backing ports are free, far above the range the daemon
// allocates, so this test cannot take a port a real sandbox is about to be given.
func freeSlot(t *testing.T) (slot, public int) {
	t.Helper()

	for slot = 400; slot < 480; slot++ {
		public = 20000 + slot*20
		if portFree(public) && portFree(public-20000+30000) {
			return slot, public
		}
	}

	t.Skip("no free test slot")

	return 0, 0
}

func portFree(port int) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}

	_ = l.Close()

	return true
}

// identity is a container's ID and start time: equal before and after means not recreated and
// not restarted.
func identity(t *testing.T, ctx context.Context, ref string) string {
	t.Helper()

	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Id}} {{.State.StartedAt}}", ref).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect %s: %v %s", ref, err, out)
	}

	return strings.TrimSpace(string(out))
}

func execIn(t *testing.T, ctx context.Context, ref, script string) string {
	t.Helper()

	out, _ := exec.CommandContext(ctx, "docker", "exec", ref, "sh", "-c", script).CombinedOutput()

	return string(out)
}

// fetch runs busybox wget through the proxy the container was configured with. The output
// carries the status line of a refusal ("403 Forbidden"); a success or an upstream that is
// unreachable from this machine are both "not 403", which is all the checks need - none of
// them depends on the internet being there.
func fetch(t *testing.T, ctx context.Context, ref, url string) string {
	t.Helper()

	return execIn(t, ctx, ref, "wget -q -T 8 -O /dev/null "+url+" 2>&1; echo rc=$?")
}
