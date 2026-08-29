package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// fakeDocker puts a `docker` on PATH that answers the handful of questions Create asks and
// records the `run` it would have made.
//
// Create shells out rather than using the Engine API - deliberately, so an error can be copied
// and re-run - which leaves the arguments it builds untested by anything else. This is the
// cheapest way to assert them without reshaping the code to suit a test.
func fakeDocker(t *testing.T, gateway string) (dir, record string) {
	t.Helper()

	dir = t.TempDir()
	record = filepath.Join(dir, "run.args")

	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"  'network inspect')\n" +
		"    for a in \"$@\"; do [ \"$a\" = '--format' ] && { echo '" + gateway + "'; exit 0; }; done\n" +
		"    exit 0 ;;\n" +
		"esac\n" +
		"case \"$1\" in\n" +
		// The container must look absent, or Create returns early with "already exists".
		"  inspect) exit 1 ;;\n" +
		"  run) printf '%s\\n' \"$@\" > " + record + "; echo deadbeef; exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"

	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return dir, record
}

// A service has to be reachable by the name every other part of sbx calls it.
//
// `depends_on: ["db"]`, `sbx logs x db` and the key in `services` all say `db`, while docker's
// embedded DNS knows only the container - `sbx-<sandbox>-<service>`. Without an alias a spec
// that declares a dependency and then configures the dependent with `db:6379` gets `no such
// host`: the dependency is woken, correctly, and still cannot be reached.
//
// Measured on a live sandbox: `nslookup db` answered NXDOMAIN while `sbx-deplab-db` resolved,
// and the dependent scored ok=0 failed=23 dialling once a second. With the alias, ok=20 failed=0.
func TestAServiceOnASandboxNetworkAnswersToItsServiceName(t *testing.T) {
	_, record := fakeDocker(t, "127.0.0.1")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{
		Image:  "redis:7-alpine",
		Ports:  []int{6379},
		Egress: spec.EgressDeny,
	}

	err := d.Create(context.Background(), "deplab", 3, 0, "db", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20061}}, t.TempDir(), IsolationContainer)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, rerr := os.ReadFile(record)
	if rerr != nil {
		t.Fatalf("no docker run was recorded: %v", rerr)
	}

	args := strings.Split(strings.TrimSpace(string(got)), "\n")

	if !hasPair(args, "--network-alias", "db") {
		t.Errorf("no `--network-alias db`, so the service answers only to sbx-deplab-db and a "+
			"dependent configured with `db:6379` gets `no such host`:\n%s", strings.Join(args, " "))
	}

	if !hasPair(args, "--network", "sbx-noegress-deplab") {
		t.Errorf("not attached to the sandbox network:\n%s", strings.Join(args, " "))
	}
}

// A service with no egress policy is on the default bridge, which has no embedded DNS at all -
// an alias there is not merely useless, docker rejects it. So it must not be asked for.
func TestNoNetworkAliasWithoutASandboxNetwork(t *testing.T) {
	_, record := fakeDocker(t, "127.0.0.1")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "redis:7-alpine", Ports: []int{6379}}

	if err := d.Create(context.Background(), "plain", 3, 0, "db", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20061}}, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, _ := os.ReadFile(record)
	args := strings.Split(strings.TrimSpace(string(got)), "\n")

	for _, a := range args {
		if a == "--network-alias" {
			t.Errorf("asked docker for a network alias on the default bridge, which it "+
				"refuses:\n%s", strings.Join(args, " "))
		}
	}
}

// egress_allow is enforced by a proxy the daemon binds ON the sandbox's gateway. Where that
// address is not one this machine can bind - a VM-backed docker keeps the bridge inside the VM -
// the proxy never starts and the service gets no egress at all, not even to the allowed hosts.
//
// Left to the daemon that was a warning every refresh tick against a sandbox reported created.
// Measured from inside such a box: api.anthropic.com -> 000, example.com -> 000. It fails
// closed, which is the safe direction and the wrong report, so create refuses instead.
func TestEgressAllowRefusesWhenItsProxyCouldNotBeBound(t *testing.T) {
	// TEST-NET-1: reserved for documentation, so it is never an address this host holds.
	fakeDocker(t, "192.0.2.1")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{
		Image:       "python:3.12",
		Ports:       []int{8000},
		EgressAllow: []string{"api.anthropic.com"},
	}

	err := d.Create(context.Background(), "agentlab", 3, 0, "agent", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer)
	if err == nil {
		t.Fatal("created a sandbox whose allow-list cannot be enforced; the service would come " +
			"up healthy with no egress at all and nothing would say so")
	}

	for _, want := range []string{"egress_allow", "192.0.2.1", "VM-backed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not explain itself: %v", want, err)
		}
	}
}

// The same allow-list on a gateway this machine can bind is created, not refused - the check
// must not become a blanket refusal of egress_allow.
func TestEgressAllowIsAllowedWhereTheProxyCanBind(t *testing.T) {
	_, record := fakeDocker(t, "127.0.0.1")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{
		Image:       "python:3.12",
		Ports:       []int{8000},
		EgressAllow: []string{"api.anthropic.com"},
	}

	if err := d.Create(context.Background(), "agentlab", 3, 0, "agent", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("refused an allow-list whose proxy can be bound: %v", err)
	}

	got, _ := os.ReadFile(record)
	if !strings.Contains(string(got), "HTTPS_PROXY=http://127.0.0.1:") {
		t.Errorf("the service was not pointed at the filtering proxy:\n%s", got)
	}
}

func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}

	return false
}
