package provider

import (
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// Two sandboxes on different slots must never be handed the same local address. This is the
// property that hashing branch names into slots failed: two of the first six branch names
// tried collided, and the two sandboxes then fought over ports.
func TestDockerSlotsDoNotCollide(t *testing.T) {
	d := newDocker(dockerEndpoint{Network: "unix", Address: "/x"})

	zero := d.Endpoints("a", "mysql", 0, 0, []int{3306})
	one := d.Endpoints("b", "mysql", 1, 0, []int{3306})

	if zero[0] == one[0] {
		t.Fatalf("slots 0 and 1 both got %s", zero[0])
	}

	if one[0].Port-zero[0].Port != blockSize {
		t.Errorf("slots are %d apart, want %d", one[0].Port-zero[0].Port, blockSize)
	}
}

// The point of the provider seam: the same spec addresses differently, and a cluster needs
// no port arithmetic at all because a pod has its own address.
func TestKubeAddressesByNameOnTheRealPort(t *testing.T) {
	k := newKube("sbx")

	eps := k.Endpoints("feature-x", "mysql", 7, 3, []int{3306})

	if eps[0].Port != 3306 {
		t.Errorf("port = %d, want the container port 3306 - no remapping in a cluster", eps[0].Port)
	}

	if want := "sbx-feature-x-mysql.sbx.svc.cluster.local"; eps[0].Host != want {
		t.Errorf("host = %q, want %q", eps[0].Host, want)
	}
}

// Isolation is a declared choice that has to reach the runtime, or it is decoration.
func TestIsolationMapsToARuntime(t *testing.T) {
	cases := []struct {
		iso          Isolation
		docker, kube string
	}{
		{IsolationContainer, "", ""},
		{IsolationGVisor, "runsc", "gvisor"},
		{IsolationKata, "kata-runtime", "kata"},
		// A microVM per pod is kata's Firecracker handler on a cluster. docker has no runtime
		// for it - locally a microVM is the firecracker provider - so it has no mapping there.
		{IsolationFirecracker, "", "kata-fc"},
	}

	for _, c := range cases {
		if got := dockerRuntime(c.iso); got != c.docker {
			t.Errorf("docker runtime for %s = %q, want %q", c.iso, got, c.docker)
		}

		if got := kubeRuntimeClass(c.iso); got != c.kube {
			t.Errorf("kube runtimeClass for %s = %q, want %q", c.iso, got, c.kube)
		}
	}

	if Isolation("none").Valid() {
		t.Error("an unknown isolation tier must not validate - silently weaker isolation than asked for is the failure that matters")
	}
}

// Two constants in two packages that must agree, with nothing linking them.
//
// spec.MaxOrdinals bounds how many ports one sandbox may claim; blockSize is how many the
// provider reserves per slot. If MaxOrdinals ever exceeds blockSize, sandbox N's ordinals
// land inside sandbox N+1's block and one sandbox answers with another's data - which is the
// failure scripts/e2e.sh exists to rule out, produced by a one-line change to an apparently
// local constant.
//
// The seam is right: the spec assigns ordinals, the provider assigns addresses. The invariant
// that makes it safe was undocumented and unasserted.
func TestOrdinalsFitInAPortBlock(t *testing.T) {
	if spec.MaxOrdinals > blockSize {
		t.Fatalf("spec.MaxOrdinals is %d and a provider slot reserves %d ports - a sandbox "+
			"can now claim addresses inside the next sandbox's block",
			spec.MaxOrdinals, blockSize)
	}
}

// The RuntimeClass name is the cluster's choice; kata-deploy's default is only a default.
func TestFirecrackerRuntimeClassIsConfigurable(t *testing.T) {
	t.Setenv("SBX_KATA_FC_RUNTIMECLASS", "kata-firecracker")

	if got := kubeRuntimeClass(IsolationFirecracker); got != "kata-firecracker" {
		t.Fatalf("runtimeClass = %q, want the configured kata-firecracker", got)
	}
}

// docker cannot give a container a microVM, and must not run one as a container instead.
func TestDockerRefusesFirecrackerIsolation(t *testing.T) {
	err := dockerRefuses(IsolationFirecracker)
	if err == nil || !strings.Contains(err.Error(), "--provider firecracker") || !strings.Contains(err.Error(), "--provider kubernetes") {
		t.Fatalf("got %v: the refusal must name both ways that do work", err)
	}

	for _, iso := range []Isolation{IsolationContainer, IsolationGVisor, IsolationKata} {
		if dockerRefuses(iso) != nil {
			t.Errorf("%s refused", iso)
		}
	}
}
