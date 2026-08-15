package provider

import "testing"

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
		t.Errorf("port = %d, want the container port 3306 — no remapping in a cluster", eps[0].Port)
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
		t.Error("an unknown isolation tier must not validate — silently weaker isolation than asked for is the failure that matters")
	}
}
