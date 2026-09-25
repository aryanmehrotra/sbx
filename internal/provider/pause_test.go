package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// A cluster cannot freeze a pod with its memory kept, so it must say so rather than scale to
// zero under the name "pause" - that would kill the background process pause exists to keep.
func TestKubernetesDoesNotClaimToPause(t *testing.T) {
	k := &kubeProvider{namespace: "sbx"}

	if _, err := PauserFor(k); err == nil || !strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("PauserFor(kubernetes) = %v, want a refusal naming the backend", err)
	}

	if _, err := InjectorFor(k); err == nil || !strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("InjectorFor(kubernetes) = %v, want a refusal naming the backend", err)
	}

	d := newDocker(dockerEndpoint{})
	if _, err := PauserFor(d); err != nil {
		t.Fatalf("docker should pause: %v", err)
	}

	if _, err := InjectorFor(d); err != nil {
		t.Fatalf("docker should inject: %v", err)
	}
}

func TestKubernetesRefusesFreezeAndReadOnlyVolumes(t *testing.T) {
	k := &kubeProvider{namespace: "sbx"}

	for field, svc := range map[string]spec.Service{
		"on_idle":          {Image: "alpine", Ports: []int{80}, OnIdle: spec.OnIdleFreeze},
		"readonly_volumes": {Image: "alpine", Ports: []int{80}, ReadOnlyVolumes: map[string]string{"v": "/opt"}},
	} {
		err := k.Create(context.Background(), "sb", 0, 0, "svc", svc, nil, "", IsolationContainer)
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: kubernetes create = %v, want a refusal naming the field", field, err)
		}
	}
}

// Entrypoint is the whole command in a pod - there is no one-word --entrypoint to split around.
func TestKubernetesEmitsEntrypointAsCommand(t *testing.T) {
	k := &kubeProvider{namespace: "sbx"}

	dep := k.deployment("x", map[string]string{}, spec.Service{
		Image: "alpine", Ports: []int{80},
		Entrypoint: []string{"/opt/sbx/sbx", "execd"}, Args: []string{"--", "tail"},
	}, IsolationContainer)

	spec := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	c := spec["containers"].([]any)[0].(map[string]any)

	cmd, _ := c["command"].([]string)
	if len(cmd) != 2 || cmd[0] != "/opt/sbx/sbx" || cmd[1] != "execd" {
		t.Fatalf("command = %v, want the entrypoint", c["command"])
	}
}
