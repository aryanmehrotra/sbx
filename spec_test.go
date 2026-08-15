package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSpec(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// Layout must not depend on which services were actually created. It used to, and skipping
// the optional ClickHouse moved MySQL's ordinal — so every config that had recorded where
// the database lived was quietly pointing somewhere else.
func TestAssignIsStableAcrossOptionalServices(t *testing.T) {
	path := writeSpec(t, `{
	  "version": 1,
	  "services": {
	    "clickhouse": {"image": "ch", "ports": [9000, 8123], "optional": true},
	    "mysql":      {"image": "my", "ports": [3306]},
	    "redis":      {"image": "rd", "ports": [6379]}
	  }
	}`)

	spec, err := LoadSpec(path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := spec.assign()
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]int{"clickhouse:9000": 0, "clickhouse:8123": 1, "mysql:3306": 2, "redis:6379": 3}

	if len(got) != len(want) {
		t.Fatalf("got %d assignments, want %d", len(got), len(want))
	}

	for _, a := range got {
		key := a.Service + ":" + itoa(a.Container)

		if want[key] != a.Index {
			t.Errorf("%s ordinal = %d, want %d", key, a.Index, want[key])
		}
	}
}

// Two sandboxes on different slots must never be handed the same local address. This is
// the property that hashing names into slots failed: `auth-flow` and `naveen-reveiw`
// collided on the first six branch names tried, and the two then fought over ports.
func TestDockerSlotsDoNotCollide(t *testing.T) {
	d := newDockerProvider(dockerEndpoint{Network: "unix", Address: "/x"})

	zero := d.Endpoints("a", "mysql", 0, 0, []int{3306})
	one := d.Endpoints("b", "mysql", 1, 0, []int{3306})

	if zero[0] == one[0] {
		t.Fatalf("slots 0 and 1 both got %s", zero[0])
	}

	if one[0].Port-zero[0].Port != blockSize {
		t.Errorf("slots are %d apart, want %d", one[0].Port-zero[0].Port, blockSize)
	}
}

// The point of the provider seam: the same spec addresses differently, and the cluster
// needs no port arithmetic at all because a pod has its own address.
func TestKubeAddressesByNameOnTheRealPort(t *testing.T) {
	k := newKubeProvider("sbx")

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

	if Isolation("none").valid() {
		t.Error("an unknown isolation tier must not validate — silently weaker isolation than asked for is the failure that matters")
	}
}

func TestLoadSpecRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"no version":        `{"services":{"a":{"image":"i","ports":[1]}}}`,
		"future version":    `{"version":99,"services":{"a":{"image":"i","ports":[1]}}}`,
		"no services":       `{"version":1,"services":{}}`,
		"no image":          `{"version":1,"services":{"a":{"ports":[1]}}}`,
		"no ports":          `{"version":1,"services":{"a":{"image":"i"}}}`,
		"port out of range": `{"version":1,"services":{"a":{"image":"i","ports":[70000]}}}`,
		// A typo in a spec must fail at load, not silently do nothing for a week.
		"unknown field": `{"version":1,"services":{"a":{"image":"i","ports":[1],"helth":"x"}}}`,
		// An export naming something that does not exist would otherwise print an empty
		// port and let a caller connect to nothing.
		"export to unknown service": `{"version":1,"services":{"a":{"image":"i","ports":[1]}},"exports":{"P":"b:1"}}`,
		"export to unexposed port":  `{"version":1,"services":{"a":{"image":"i","ports":[1]}},"exports":{"P":"a:2"}}`,
		"export without port":       `{"version":1,"services":{"a":{"image":"i","ports":[1]}},"exports":{"P":"a"}}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadSpec(writeSpec(t, body)); err == nil {
				t.Fatalf("LoadSpec accepted %s", name)
			}
		})
	}
}

func TestAssignRefusesToOverflowTheBlock(t *testing.T) {
	// 21 ports in a 20-port block: better a clear error than a service silently landing
	// in the next sandbox's range.
	body := `{"version":1,"services":{"a":{"image":"i","ports":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21]}}}`

	spec, err := LoadSpec(writeSpec(t, body))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := spec.assign(); err == nil {
		t.Fatal("assign accepted more ports than the block holds")
	}
}
