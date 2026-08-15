package provider

// The kubernetes provider's only security check, which had no test.
//
// `egress: "deny"` is enforced on docker by a bridge with IP masquerade disabled. In a
// cluster the equivalent is a NetworkPolicy, and a NetworkPolicy is only enforced by some
// CNIs — so applying one and reporting success would leave a service fully open on a cluster
// whose CNI ignores it, while the spec said deny and the output said fine.
//
// So it refuses. DECISIONS.md: a security control that silently did nothing is worse than
// one that says no. This holds that, and it runs without a cluster because the check happens
// before any kubectl call.

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

func TestKubernetesRefusesEgressDeny(t *testing.T) {
	p := &kubeProvider{namespace: "sbx"}

	svc := spec.Service{
		Image:  "nginx:alpine",
		Ports:  []int{80},
		Egress: spec.EgressDeny,
	}

	err := p.Create(context.Background(), "sandbox", 0, 0, "web", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20000}}, ".", IsolationContainer)

	if err == nil {
		t.Fatal("a service declaring egress deny was created on kubernetes — the spec asked " +
			"for no egress and got a service that can reach anything")
	}

	// It has to say which service and why, or the operator is left guessing which of a
	// dozen services in the spec is the problem.
	for _, want := range []string{"web", "egress", "NetworkPolicy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// The refusal must be specific to egress deny, not a blanket failure that happens to look
// right. A service with no egress declaration must get past this check.
func TestKubernetesDoesNotRefuseAServiceWithoutEgressDeny(t *testing.T) {
	p := &kubeProvider{namespace: "sbx"}

	svc := spec.Service{Image: "nginx:alpine", Ports: []int{80}}

	err := p.Create(context.Background(), "sandbox", 0, 0, "web", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20000}}, ".", IsolationContainer)

	// It will fail — there is no cluster here — but it must not fail for the egress reason,
	// or this provider refuses everything and the test above proves nothing.
	if err != nil && strings.Contains(err.Error(), "egress") {
		t.Errorf("a service with no egress declaration was refused for egress: %v", err)
	}
}

// The cache that keeps the wake poll from forking kubectl on every iteration, and the
// invalidation that keeps it honest.
//
// Deployment names are reused — `sbx rm x && sbx create x` builds the same name with a
// possibly different readiness command — so a cache keyed by name and never cleared would
// probe a recreated deployment with the old command, on the wake path, for the life of the
// process. Create is the only thing that changes it, so Create is what clears it.
func TestReadinessCacheIsInvalidatedWhenTheDeploymentIsRecreated(t *testing.T) {
	k := newKube("sbx")

	// Standing in for a first lookup that found a command.
	k.mu.Lock()
	k.ready["sbx-b-redis"] = "redis-cli ping"
	k.mu.Unlock()

	if cmd, ok := k.cachedReady("sbx-b-redis"); !ok || cmd != "redis-cli ping" {
		t.Fatalf("cachedReady = (%q, %v), want the cached command", cmd, ok)
	}

	k.forgetReady("sbx-b-redis")

	k.mu.Lock()
	_, still := k.ready["sbx-b-redis"]
	k.mu.Unlock()

	if still {
		t.Error("the readiness command survived a recreate — a probe would use the old one")
	}
}

// A deployment that genuinely declares no readiness command is remembered as such, so the
// poll loop does not re-ask kubectl every 100 ms for an answer that will not change.
func TestAnAbsentReadinessCommandIsRemembered(t *testing.T) {
	k := newKube("sbx")

	k.mu.Lock()
	k.ready["sbx-b-web"] = ""
	k.mu.Unlock()

	if _, ok := k.cachedReady("sbx-b-web"); ok {
		t.Error("an empty readiness command was reported as declared")
	}
}
