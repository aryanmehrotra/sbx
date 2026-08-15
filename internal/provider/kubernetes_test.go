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
