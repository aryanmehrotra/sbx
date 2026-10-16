package provider

import (
	"slices"
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

// One parser, so a tier named on `--isolation` is named the same through the OpenSandbox API.
func TestParseIsolationTakesEveryTierAndRefusesTheRest(t *testing.T) {
	for _, want := range []Isolation{IsolationContainer, IsolationGVisor, IsolationKata, IsolationFirecracker} {
		got, err := ParseIsolation(string(want))
		if err != nil || got != want {
			t.Errorf("ParseIsolation(%q) = %q, %v", want, got, err)
		}
	}

	for _, bad := range []string{"", "jail", "Container", "runsc", " gvisor"} {
		if _, err := ParseIsolation(bad); err == nil {
			t.Errorf("ParseIsolation(%q) accepted", bad)
		} else if !strings.Contains(err.Error(), "gvisor") {
			// The message lists the tiers, because the caller got the spelling wrong and the
			// list is the fix.
			t.Errorf("ParseIsolation(%q): %v - the refusal must list the tiers", bad, err)
		}
	}
}

// The tier a caller asks for is checked against the host before anything is created, and a tier
// this docker cannot give is refused rather than quietly served as a container.
func TestProvidesIsolationRefusesWhatDockerCannotRun(t *testing.T) {
	// No engine behind it: the gvisor and kata branches below never get that far.
	d := &dockerProvider{endpoint: dockerEndpoint{Network: "unix", Address: "/nonexistent/docker.sock"}}

	var _ IsolationHost = d

	// A container is the daemon's own runtime, so there is nothing to look for and no engine to
	// ask - this must not refuse, because it is the default every existing create takes.
	if err := d.ProvidesIsolation(IsolationContainer); err != nil {
		t.Errorf("container refused: %v", err)
	}

	// A microVM per container is not a runtime docker can be handed, which is true whether or
	// not an engine answers.
	if err := d.ProvidesIsolation(IsolationFirecracker); err == nil ||
		!strings.Contains(err.Error(), "--provider firecracker") {
		t.Errorf("firecracker: %v - want the refusal that names what does work", err)
	}

	// No engine to judge: whatever asks next reports a dead daemon better than an isolation
	// message would, so this is not the place that refuses.
	//
	// This reaches that arm two ways, and both must be silent. Against an unreachable socket
	// `docker info` exits non-zero on some versions and ZERO on others, printing client-side
	// info and warning on stderr. The second shape is why the name list, not the exit code, is
	// what decides: see TestDockerRuntimeNamesTreatsAnEmptyAnswerAsNoAnswer below.
	for _, iso := range []Isolation{IsolationGVisor, IsolationKata} {
		if err := d.ProvidesIsolation(iso); err != nil {
			t.Errorf("%s refused with no engine to ask: %v", iso, err)
		}
	}
}

// An empty answer is not a negative answer.
//
// A daemon that is up always reports at least its own default runtime, so no names means the
// question never reached a daemon — and refusing on that would tell an operator whose daemon is
// down to install a runtime they may already have. That is the same misdiagnosis
// ProvidesIsolation exists to remove, reached from the other side.
//
// Pinned here rather than through ProvidesIsolation because the docker call shells out with no
// seam, and the shape of the answer is what the decision turns on.
func TestDockerRuntimeNamesTreatsAnEmptyAnswerAsNoAnswer(t *testing.T) {
	for name, info := range map[string]string{
		"nothing at all":         "",
		"an empty object":        "{}",
		"a null":                 "null",
		"output nobody can read": "Cannot connect to the Docker daemon",
	} {
		t.Run(name, func(t *testing.T) {
			if got := dockerRuntimeNames(info); len(got) != 0 {
				t.Errorf("dockerRuntimeNames(%q) = %v, want none: a non-empty list here would be "+
					"read as a daemon that answered and lacks the runtime", info, got)
			}
		})
	}
}

func TestDockerRuntimeNamesReadsTheDaemonsRegisteredRuntimes(t *testing.T) {
	const info = `{"io.containerd.runc.v2":{"path":"runc"},"runsc":{"path":"/usr/local/bin/runsc"}}`

	got := dockerRuntimeNames(info)
	if !slices.Equal(got, []string{"io.containerd.runc.v2", "runsc"}) {
		t.Fatalf("names = %v, want them sorted", got)
	}

	// Unreadable output is no names, which reads as "not registered": the alternative is
	// accepting a tier on the strength of output nobody could parse.
	for _, out := range []string{"", "not json", "null"} {
		if n := dockerRuntimeNames(out); len(n) != 0 {
			t.Errorf("dockerRuntimeNames(%q) = %v", out, n)
		}
	}

	if listed(nil) == listed([]string{"runc"}) {
		t.Error("listed() does not distinguish no runtimes from some")
	}
}
