package provider

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/osbclient"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// TestFirecrackerPoolE2E parks a real API microVM as a warm-pool member - asleep, then frozen -
// and claims it: the claimed execd must answer the caller's token with the caller's env, and
// refuse the token the member booted with. Timings are logged: claim asleep (restore + re-key)
// against claim frozen (resume + re-key) is the measurement the pool's default rests on.
//
// Opt-in and Linux-only, as TestFirecrackerE2E (SBX_FC_E2E=1, root, SBX_EXECD_BINARY).
func TestFirecrackerPoolE2E(t *testing.T) {
	if os.Getenv("SBX_FC_E2E") != "1" {
		t.Skip("SBX_FC_E2E=1 to boot a real Firecracker VM")
	}

	image := os.Getenv("SBX_FC_E2E_IMAGE")
	if image == "" {
		image = "redis:7-alpine"
	}

	prov, err := For("firecracker", "", "")
	if err != nil {
		t.Fatal(err)
	}

	p := prov.(*fcProvider)
	ctx := context.Background()

	for _, frozen := range []bool{false, true} {
		mode := map[bool]string{false: "asleep", true: "frozen"}[frozen]
		sandbox := fmt.Sprintf("osb-fcpool%d", time.Now().UnixNano()%100000)

		slot, err := p.AllocSlot(ctx, sandbox)
		if err != nil {
			t.Fatal(err)
		}

		svc := spec.Service{Image: image, Ports: []int{44772}, OSBOwner: "sbx-e2e",
			Entrypoint: []string{"tail", "-f", "/dev/null"}, OnIdle: spec.OnIdleFreeze,
			Env: map[string]string{AgentTokenEnv: "member-token-" + mode}}
		eps := p.Endpoints(sandbox, "sandbox", slot, 0, svc.Ports)

		t.Cleanup(func() { _ = p.Remove(context.WithoutCancel(ctx), sandbox) })

		start := time.Now()
		if err := p.Create(ctx, sandbox, slot, 0, "sandbox", svc, eps, "", IsolationContainer); err != nil {
			t.Fatal(err)
		}

		ref := containerName(sandbox, "sandbox")
		t.Logf("%s: create (born running): %s", mode, time.Since(start))

		start = time.Now()
		if err := p.Park(ctx, ref, frozen); err != nil {
			t.Fatalf("%s: park: %v\n%s", mode, err, p.consoleTail(p.dir(ref), 30))
		}

		t.Logf("%s: park: %s", mode, time.Since(start))

		start = time.Now()
		if err := p.Claim(ctx, ref, "caller-token-"+mode, map[string]string{"POOLED": mode}); err != nil {
			t.Fatalf("%s: claim: %v\n%s", mode, err, p.consoleTail(p.dir(ref), 30))
		}

		claimed := time.Since(start)

		out, err := p.Exec(ctx, ref, []string{"sh", "-c", "echo $POOLED"})
		if err != nil || out != mode {
			t.Fatalf("%s: exec with the caller's token: %q, %v - want the claim's env", mode, out, err)
		}

		t.Logf("%s: claim %s, claim to first command %s", mode, claimed, time.Since(start))

		hc, _, err := p.execdFor(ref, "e2e")
		if err != nil {
			t.Fatal(err)
		}

		// Not /ping, which answers without a token: a command, which needs one.
		old := osbclient.NewExecd(execdBase, "member-token-"+mode, hc)
		if err := old.StreamCommand(ctx, osbclient.CommandRequest{Argv: []string{"true"}},
			func(osbclient.Event) error { return nil }); err == nil {
			t.Fatalf("%s: the claimed execd still answers the member's token", mode)
		}
	}
}
