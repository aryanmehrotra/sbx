package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/egress"
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

// A service with an egress_policy - including a default-ALLOW one - goes on the no-NAT bridge
// behind the filter, with its declared policy on a label for the daemon. Default-allow is the
// case that matters: were it left on the ordinary bridge its deny rules would be advisory, since
// a client could dial round the proxy.
func TestAnEgressPolicyPutsTheServiceBehindTheFilter(t *testing.T) {
	_, record := fakeDocker(t, "127.0.0.1")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{
		Image: "python:3.12",
		Ports: []int{8000},
		EgressPolicy: &egress.Policy{DefaultAction: "allow", Egress: []egress.Rule{
			{Action: "deny", Target: "169.254.0.0/16"},
		}},
	}

	if err := d.Create(context.Background(), "osblab", 3, 0, "sandbox", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, _ := os.ReadFile(record)
	args := strings.Split(strings.TrimSpace(string(got)), "\n")

	if !hasPair(args, "--network", "sbx-noegress-osblab") {
		t.Errorf("a default-allow policy was not put on the no-NAT bridge, so its deny rules "+
			"could be walked around:\n%s", strings.Join(args, " "))
	}

	want := labelEgressPolicy + `={"defaultAction":"allow","egress":[{"action":"deny","target":"169.254.0.0/16"}]}`
	if !hasPair(args, "--label", want) {
		t.Errorf("the declared policy is not on the container for the daemon to read:\n%s",
			strings.Join(args, "\n"))
	}

	if !strings.Contains(string(got), "HTTPS_PROXY=http://127.0.0.1:") {
		t.Errorf("the service was not pointed at the filtering proxy:\n%s", got)
	}
}

func TestKubernetesRefusesAnEgressFilter(t *testing.T) {
	k := &kubeProvider{}

	for _, svc := range []spec.Service{
		{Image: "x", Egress: "allow"},
		{Image: "x", EgressAllow: []string{"a.com"}},
		{Image: "x", EgressPolicy: &egress.Policy{DefaultAction: "deny"}},
	} {
		err := k.Create(context.Background(), "s", 0, 0, "svc", svc, nil, "", IsolationContainer)
		if err == nil || !strings.Contains(err.Error(), "egress filter") {
			t.Errorf("%+v: kubernetes created a filtered service it cannot filter (err %v)", svc, err)
		}
	}
}

// The sandbox's bridge and the filter's address on it are decided in two places - `network create
// --subnet` in ensureEgressNetwork, and `docker run --ip` in ensureFilterContainer, off whatever
// lastUsable makes of the subnet docker reports - and nothing but this test makes them agree.
//
// They did not. The bridge was created without --subnet, so docker's IPAM chose one, and from
// Engine 28 docker refuses --ip on a network it addressed itself: "user specified IP address is
// supported only when connecting to networks with user configured subnets" (Docker Desktop
// 28.5.1). The filter container could not start at all, which took `egress_policy`, `egress_allow`
// and `egress: "allow"` with it on every VM-backed docker.
func TestTheEgressBridgeSubnetHoldsTheFiltersFixedAddress(t *testing.T) {
	block := netip.MustParsePrefix(egressSubnetBlock)

	for _, sandbox := range []string{"fx", "deplab", "osblab", "", "a-very-long-sandbox-name-9"} {
		for try := range egressSubnetTries {
			subnet := egressSubnet(sandbox, try)

			if !block.Overlaps(subnet) || subnet.Bits() != egressSubnetBits {
				t.Fatalf("egressSubnet(%q, %d) = %s, which is not a /%d of %s",
					sandbox, try, subnet, egressSubnetBits, egressSubnetBlock)
			}

			ip, err := lastUsable(subnet.String())
			if err != nil {
				t.Fatalf("egressSubnet(%q, %d) = %s has no address for the filter: %v",
					sandbox, try, subnet, err)
			}

			addr, err := netip.ParseAddr(ip)
			if err != nil {
				t.Fatalf("lastUsable(%s) = %q: %v", subnet, ip, err)
			}

			if !subnet.Contains(addr) {
				t.Fatalf("the filter would be given %s, which is not on its bridge %s - docker "+
					"refuses an --ip outside the network's subnet", addr, subnet)
			}

			// Not the gateway (docker holds it), not the network or broadcast address.
			gateway := subnet.Masked().Addr().Next()
			if addr == gateway || addr == subnet.Masked().Addr() {
				t.Fatalf("the filter's address %s on %s is the gateway or the network address",
					addr, subnet)
			}
		}
	}
}

// Where sbx puts its bridges. Docker's own default address pools are 172.16.0.0/12 and
// 192.168.0.0/16, so a subnet taken from either would compete with the networks docker assigns
// itself; the rest are ranges a laptop, a VPN or a cloud VPC really uses, and a bridge shadowing
// one would take that range away from the filter, which is the only container with a route out.
func TestTheEgressBlockIsNoOnesAddressSpace(t *testing.T) {
	block := netip.MustParsePrefix(egressSubnetBlock)

	for _, theirs := range []string{
		"10.0.0.0/8",     // VPNs and VPCs
		"172.16.0.0/12",  // docker's default pool, and RFC 1918
		"192.168.0.0/16", // home LANs, and docker's second pool
		"100.64.0.0/10",  // CGNAT and tailscale; egress.Private already refuses it
		"169.254.0.0/16", // link-local, including the cloud metadata address
		"127.0.0.0/8",    // loopback
		"224.0.0.0/4",    // multicast
	} {
		if p := netip.MustParsePrefix(theirs); p.Overlaps(block) {
			t.Errorf("sbx's egress block %s overlaps %s, which is someone else's", block, theirs)
		}
	}

	if !inEgressBlock(egressSubnet("fx", 0).String()) {
		t.Error("inEgressBlock does not recognise a subnet egressSubnet just handed out")
	}

	// A bridge from an sbx that let docker's IPAM choose: not ours, and the one docker 28
	// refuses the filter's --ip on.
	for _, legacy := range []string{"172.30.0.0/16", "192.168.112.0/20", "", "nonsense"} {
		if inEgressBlock(legacy) {
			t.Errorf("inEgressBlock(%q) is true, so a bridge sbx did not address would be kept "+
				"and its filter would not start", legacy)
		}
	}
}

// The same invariant through the two commands sbx actually runs, against a `docker` that remembers
// the subnet it was created with - which is what a real docker does, and what makes the `--ip` it
// is handed next either legal or refused.
func TestTheFilterIsGivenAnAddressOnTheSubnetSbxAskedFor(t *testing.T) {
	calls := subnetDocker(t)

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, Egress: spec.EgressAllow}

	if err := d.Create(context.Background(), "fresh", 3, 0, "app", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("create: %v", err)
	}

	log := callLog(t, calls)

	create, ok := calledWith(log, "network create ")
	if !ok {
		t.Fatalf("the sandbox bridge was never created:\n%s", strings.Join(log, "\n"))
	}

	subnet := argAfter(create, "--subnet")
	if subnet == "" {
		t.Fatalf("the bridge was created without --subnet, so docker's IPAM addressed it and "+
			"docker 28 refuses the filter's --ip on it:\n%s", create)
	}

	run, ok := calledWith(log, "run -d --name sbx-egressfilter-fresh")
	if !ok {
		t.Fatalf("no filter container was started:\n%s", strings.Join(log, "\n"))
	}

	want, err := lastUsable(subnet)
	if err != nil {
		t.Fatalf("lastUsable(%s): %v", subnet, err)
	}

	if got := argAfter(run, "--ip"); got != want {
		t.Fatalf("the bridge was created as %s and the filter asked for %s, want %s: docker "+
			"refuses an address off the subnet", subnet, got, want)
	}

	// And the gateway, for the same reason one step along: docker fills IPAM.Config[0].Gateway in
	// only when it chose the subnet, and that field is where egressGateway, HTTP_PROXY on native
	// docker and every filter's -refuse list come from.
	p, err := netip.ParsePrefix(subnet)
	if err != nil {
		t.Fatal(err)
	}

	if got := argAfter(create, "--gateway"); got != egressBridgeGateway(p) {
		t.Fatalf("the bridge was created with gateway %q, want %s - docker reports no gateway "+
			"for a subnet it did not choose, and create fails with \"no gateway address\"",
			got, egressBridgeGateway(p))
	}

	if want == egressBridgeGateway(p) {
		t.Fatalf("the filter was given the bridge's own gateway %s", want)
	}
}

// A bridge left behind by an sbx that let docker choose its subnet is replaced, so upgrading is
// not "every filtered sandbox on this machine stays broken until you remove it by hand". Only
// when nothing is attached: `network rm` refuses a network in use, and that refusal is the check.
func TestABridgeDockerAddressedItselfIsReplaced(t *testing.T) {
	calls := subnetDocker(t)

	// What the older sbx left: the network exists, with a subnet out of docker's pool.
	writeSubnet(t, calls, "172.30.0.0/16")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, Egress: spec.EgressAllow}

	out := captureStdout(t, func() {
		if err := d.Create(context.Background(), "fresh", 3, 0, "app", svc,
			[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err != nil {
			t.Fatalf("create: %v", err)
		}
	})

	log := callLog(t, calls)

	if _, ok := calledWith(log, "network rm sbx-noegress-fresh"); !ok {
		t.Fatalf("a bridge docker addressed itself was kept, and its filter cannot start:\n%s",
			strings.Join(log, "\n"))
	}

	create, ok := calledWith(log, "network create ")
	if !ok || !inEgressBlock(argAfter(create, "--subnet")) {
		t.Fatalf("the bridge was not recreated in sbx's own block:\n%s", strings.Join(log, "\n"))
	}

	if !strings.Contains(out, "replacing the egress bridge for fresh") {
		t.Errorf("the replacement happened without saying why: %q", out)
	}
}

// A bridge sbx already addressed is left alone - the replacement must not become "recreate the
// sandbox network on every create", which would detach nothing but would churn the one network
// the sandbox's services share.
func TestABridgeSbxAlreadyAddressedIsKept(t *testing.T) {
	calls := subnetDocker(t)
	writeSubnet(t, calls, egressSubnet("fresh", 0).String())

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, Egress: spec.EgressAllow}

	if err := d.Create(context.Background(), "fresh", 3, 0, "app", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("create: %v", err)
	}

	log := callLog(t, calls)

	if c, ok := calledWith(log, "network rm"); ok {
		t.Fatalf("the sandbox's own bridge was removed and recreated (%s):\n%s", c,
			strings.Join(log, "\n"))
	}

	if c, ok := calledWith(log, "network create "); ok {
		t.Fatalf("a bridge that was already there was created again (%s)", c)
	}
}

// subnetDocker is a `docker` on PATH that keeps the one piece of state this is about: the subnet
// the sandbox's bridge was created with. `network inspect` answers from it, so the filter's --ip
// is computed from the subnet the create asked for, exactly as it is against a real docker.
//
// The gateway it reports is the bridge's own .1, which no machine running this test holds, so
// Create takes the container-filter path on Linux and macOS alike.
func subnetDocker(t *testing.T) (calls string) {
	t.Helper()

	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	sub, gw := filepath.Join(dir, "subnet"), filepath.Join(dir, "gateway")
	ours, on := filepath.Join(dir, "ours"), filepath.Join(dir, "attached")
	up, svcs := filepath.Join(dir, "filterup"), filepath.Join(dir, "services")
	had, boom := filepath.Join(dir, "filterlabels"), filepath.Join(dir, "servicefails")

	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + calls + "\n" +
		"case \"$*\" in\n" +
		"  'network ls -q') echo n1; exit 0 ;;\n" +
		"  *'network inspect --format'*) echo '172.17.0.1 172.17.0.0/16 '; exit 0 ;;\n" +
		"  *'network inspect sbx-noegress-'*Subnet*) [ -f " + sub + " ] || exit 1; cat " + sub + "; exit 0 ;;\n" +
		"  *'network inspect sbx-noegress-'*Gateway*) [ -f " + gw + " ] || exit 1; cat " + gw + "; exit 0 ;;\n" +
		"  'network create '*)\n" +
		// Docker's own IPAM when nothing asked: it picks a subnet out of its pool and reports a
		// gateway with it. That is the whole bug - the bridge looks addressed, and `--ip` on it
		// is refused anyway.
		"    printf '172.30.0.0/16\\n' > " + sub + "; printf '172.30.0.1\\n' > " + gw + "; rm -f " + ours + "\n" +
		// A subnet of our own, and docker reports back exactly the two fields it was given.
		// Ask for the subnet alone and IPAM.Config[0].Gateway comes back EMPTY, which is how
		// the first version of this fix failed: "the egress bridge for X has no gateway address".
		"    p=''\n" +
		"    for a in \"$@\"; do\n" +
		"      [ \"$p\" = '--subnet' ] && { printf '%s\\n' \"$a\" > " + sub + "; : > " + gw + "; : > " + ours + "; }\n" +
		"      [ \"$p\" = '--gateway' ] && printf '%s\\n' \"$a\" > " + gw + "\n" +
		"      p=\"$a\"\n" +
		"    done\n" +
		"    exit 0 ;;\n" +
		"  'network rm '*) rm -f " + sub + " " + gw + " " + ours + "; exit 0 ;;\n" +
		"  'network inspect '*) [ -f " + sub + " ] || exit 1 ;;\n" +
		// What the sandbox still has on it: containers attached to its bridge, and its own
		// service containers (the filter carries no sbx.service label, which is how the two
		// are told apart - the same filter `slotOf` uses).
		"  'ps -aq --filter network='*) cat " + on + " 2>/dev/null; exit 0 ;;\n" +
		"  'ps -aq --filter label=sbx.sandbox='*'label=sbx.service') cat " + svcs + " 2>/dev/null; exit 0 ;;\n" +
		// The filter container: present only once something started it (or the test said it
		// was already there). `inspect -f {{.Id}}` is removeFilterContainer asking; `inspect
		// --format <labels>` is ensureFilterContainer asking what it is enforcing.
		"  'inspect -f {{.Id}} sbx-egressfilter-'*) [ -f " + up + " ] || exit 1; echo deadbeef; exit 0 ;;\n" +
		"  'inspect --format'*sbx-egressfilter-*) [ -f " + had + " ] || exit 1; cat " + had + "; exit 0 ;;\n" +
		"  'inspect '*) exit 1 ;;\n" +
		"  'image inspect '*) exit 0 ;;\n" +
		"  'port '*) echo '127.0.0.1:41234'; exit 0 ;;\n" +
		"  'rm -f sbx-egressfilter-'*) rm -f " + up + "; exit 0 ;;\n" +
		"  'run -d --name sbx-egressfilter-'*)\n" +
		// Docker 28's own refusal, which is the whole defect the --subnet fix is about: a fixed
		// address is legal only on a subnet the user gave.
		"    [ -f " + ours + " ] || { echo 'docker: Error response from daemon: invalid endpoint " +
		"settings: user specified IP address is supported only when connecting to networks with " +
		"user configured subnets' >&2; exit 125; }\n" +
		"    : > " + up + "; echo deadbeef; exit 0 ;;\n" +
		// Any other `run` is the service's, and it is the one a test makes fail: a create that
		// got as far as the filter and then could not start what the filter was for.
		"  'run '*)\n" +
		"    [ -f " + boom + " ] && { echo 'docker: Error response from daemon: no such image' >&2; exit 125; }\n" +
		"    echo deadbeef; exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"

	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return calls
}

// mark writes one of subnetDocker's state files, which is how a test says what the engine
// already has: a service container of the sandbox, a filter that is already running, a
// `docker run` for the service that will fail.
func mark(t *testing.T, calls, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(filepath.Dir(calls), name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeSubnet makes subnetDocker's bridge already exist, addressed as given.
func writeSubnet(t *testing.T, calls, subnet string) {
	t.Helper()

	dir := filepath.Dir(calls)
	gw, err := netip.ParsePrefix(subnet)

	if err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{"subnet": subnet, "gateway": egressBridgeGateway(gw)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A bridge in sbx's own block is one sbx gave a subnet, which is what docker 28 needs before
	// it will accept the filter's --ip on it.
	if inEgressBlock(subnet) {
		if err := os.WriteFile(filepath.Join(dir, "ours"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// attachContainer puts a container on subnetDocker's bridge, the way a sleeping sandbox's own
// service containers sit on it: present, stopped, and listed by `docker ps -aq --filter network=`.
func attachContainer(t *testing.T, calls string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(filepath.Dir(calls), "attached"),
		[]byte("c0ffee\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// argAfter is the value a flag was given in one recorded `docker` call.
func argAfter(call, flag string) string {
	f := strings.Fields(call)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == flag {
			return f[i+1]
		}
	}

	return ""
}

// Re-addressing a bridge means creating it again, and a recreated network has a new id: a
// container that was on the old one does not start any more ("failed to set up container
// networking: network <old id> not found", Docker Desktop 28.5.1). Every service of a SLEEPING
// sandbox is such a container - stopped, still attached - and `docker network rm` removes a
// network out from under one without complaining. So a bridge with anything on it is left alone,
// and the filter explains itself instead.
func TestABridgeWithContainersOnItIsNotReAddressed(t *testing.T) {
	calls := subnetDocker(t)

	writeSubnet(t, calls, "172.30.0.0/16")
	attachContainer(t, calls)

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, Egress: spec.EgressAllow}

	err := d.Create(context.Background(), "fresh", 3, 0, "app", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer)

	log := callLog(t, calls)

	if c, ok := calledWith(log, "network rm"); ok {
		t.Fatalf("the bridge was removed under the sandbox's own containers (%s), which leaves "+
			"every stopped service unable to start:\n%s", c, strings.Join(log, "\n"))
	}

	// It cannot work on docker 28, and the refusal has to say what to do about it rather than
	// pass docker's "user specified IP address..." straight through.
	if err == nil {
		t.Fatal("a filter was reported started on a bridge docker 28 refuses its address on")
	}

	for _, want := range []string{"sbx rm fresh", "docker addressed rather than sbx", "172.30.255.254"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not explain itself: %v", want, err)
		}
	}
}

// A create that gets as far as starting the egress filter and then fails has to take the filter
// with it. Otherwise `sbx create` reports a failure and leaves behind a container with
// `--restart unless-stopped`, a published loopback port, a route to the internet off the default
// bridge, and the filter's control token on a label - owned by a sandbox that does not exist.
// Nothing reclaimed it: `sbx rm` answered `no sandbox "x"` because the sandbox has no units, and
// `sbx gc` lists only volumes and snapshot images.
func TestAFailedCreateTakesTheEgressFilterItStarted(t *testing.T) {
	calls := subnetDocker(t)
	mark(t, calls, "servicefails", "yes")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, Egress: spec.EgressAllow}

	err := d.Create(context.Background(), "fresh", 3, 0, "app", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer)
	if err == nil {
		t.Fatal("the service could not be started and create reported success")
	}

	log := callLog(t, calls)

	if _, ok := calledWith(log, "run -d --name sbx-egressfilter-fresh"); !ok {
		t.Fatalf("the filter was never started, so this is not the case under test:\n%s",
			strings.Join(log, "\n"))
	}

	if _, ok := calledWith(log, "rm -f sbx-egressfilter-fresh"); !ok {
		t.Errorf("a create that failed left its egress filter running, and nothing can reclaim "+
			"it: `sbx rm fresh` says there is no sandbox and `sbx gc` cannot see it:\n%s",
			strings.Join(log, "\n"))
	}

	if _, ok := calledWith(log, "network rm sbx-noegress-fresh"); !ok {
		t.Errorf("a create that failed left the bridge it made behind:\n%s", strings.Join(log, "\n"))
	}
}

// Only what this call made. A create that REUSED the sandbox's filter must not remove it when it
// fails: the filter belongs to the sandbox, not to this create, and another service of the same
// sandbox is behind it.
func TestAFailedCreateKeepsAnEgressFilterItDidNotStart(t *testing.T) {
	calls := subnetDocker(t)

	// The sandbox is already there: an addressed bridge, and a filter enforcing exactly the
	// declaration this create asks for, so ensureFilterContainer reuses it.
	writeSubnet(t, calls, egressSubnet("fresh", 0).String())
	mark(t, calls, "filterup", "yes")
	mark(t, calls, "servicefails", "yes")

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, Egress: spec.EgressAllow}

	body, err := json.Marshal(svc.DeclaredPolicy())
	if err != nil {
		t.Fatal(err)
	}

	tag, err := filterImageWant()
	if err != nil {
		t.Fatal(err)
	}

	ip, err := lastUsable(egressSubnet("fresh", 0).String())
	if err != nil {
		t.Fatal(err)
	}

	mark(t, calls, "filterlabels", string(body)+"\x1f\x1f"+ip+"\x1f"+tag+"\n")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	if err := d.Create(context.Background(), "fresh", 3, 0, "app", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err == nil {
		t.Fatal("the service could not be started and create reported success")
	}

	log := callLog(t, calls)

	if c, ok := calledWith(log, "run -d --name sbx-egressfilter-fresh"); ok {
		t.Fatalf("the filter was replaced rather than reused (%s), so this is not the case "+
			"under test:\n%s", c, strings.Join(log, "\n"))
	}

	if _, ok := calledWith(log, "rm -f sbx-egressfilter-fresh"); ok {
		t.Errorf("a failed create removed a filter it did not start, taking egress away from "+
			"the rest of the sandbox:\n%s", strings.Join(log, "\n"))
	}

	if _, ok := calledWith(log, "network rm sbx-noegress-fresh"); ok {
		t.Errorf("a failed create removed a bridge it did not make:\n%s", strings.Join(log, "\n"))
	}
}

// Nor when the sandbox still has services. `sbx create` runs service by service, so a second
// service failing after the first came up behind a replaced filter must leave that filter alone -
// removing it would take egress away from a service that is running and healthy.
func TestAFailedCreateKeepsTheFilterWhenTheSandboxStillHasAService(t *testing.T) {
	calls := subnetDocker(t)
	mark(t, calls, "servicefails", "yes")
	mark(t, calls, "services", "c0ffee\n") // an earlier service of this sandbox, already up

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, Egress: spec.EgressAllow}

	if err := d.Create(context.Background(), "fresh", 3, 0, "app", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err == nil {
		t.Fatal("the service could not be started and create reported success")
	}

	log := callLog(t, calls)

	if _, ok := calledWith(log, "rm -f sbx-egressfilter-fresh"); ok {
		t.Errorf("a failed create took the filter down while another service of the sandbox was "+
			"still behind it:\n%s", strings.Join(log, "\n"))
	}

	if _, ok := calledWith(log, "network rm sbx-noegress-fresh"); ok {
		t.Errorf("a failed create removed the bridge a running service is on:\n%s",
			strings.Join(log, "\n"))
	}
}

// `egress: "deny"` is a bridge with IP masquerade off, and docker enforces it rather than sbx.
// That only holds where the bridge is an interface of this machine: on a VM-backed docker - colima,
// Docker Desktop - the NAT is the VM boundary's, outside the VM's iptables, and a bridge with
// masquerade off was measured still reaching 1.1.1.1 and 8.8.8.8. A service that declared deny
// came up, reported healthy, and had a route out.
//
// So the refusal and the platform have to agree, whichever platform this runs on: where the
// gateway is not bindable the create must be refused, and where it is bindable deny must still
// work. Asserting "it is refused" alone would fail on native Linux docker, where deny is real -
// the invariant is that deny is never reported applied where it cannot be enforced.
func TestEgressDenyIsRefusedExactlyWhereTheBridgeCannotEnforceIt(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	sandbox := fmt.Sprintf("denyenf-%d", time.Now().UnixNano())

	t.Cleanup(func() {
		_, _ = d.docker("rm", "-f", "-v", "sbx-"+sandbox+"-app")
		_, _ = d.docker("network", "rm", egressNetwork(sandbox))
	})

	if _, err := d.ensureEgressNetwork(sandbox); err != nil {
		t.Fatalf("creating the no-egress bridge for %s: %v", sandbox, err)
	}

	gw, err := d.egressGateway(sandbox)
	if err != nil {
		t.Fatalf("reading the bridge's gateway for %s: %v", sandbox, err)
	}

	// The platform's own answer, taken the same way the create takes it.
	enforceable := bindGateway(gw) == nil

	svc := spec.Service{Image: "alpine:3", Egress: spec.EgressDeny}

	err = d.Create(ctx, sandbox, 0, 0, "app", svc, nil, t.TempDir(), IsolationContainer)

	switch {
	case enforceable && err != nil:
		t.Errorf("the bridge gateway %s is an address this machine holds, so a bridge without NAT "+
			"does stop egress here, but the create was refused anyway: %v", gw, err)
	case !enforceable && err == nil:
		t.Errorf("the bridge gateway %s is not an address this machine holds, so docker's bridges "+
			"are inside a VM that routes them out and `egress: \"deny\"` cannot be enforced - "+
			"the create reported a service denied egress while it has a route out", gw)
	case !enforceable && !strings.Contains(err.Error(), "egress deny"):
		t.Errorf("deny was refused on a bridge this machine cannot enforce, but the message does "+
			"not say which control was refused, so nobody can act on it: %v", err)
	}
}

// 198.18.0.0/15 is reserved, which is why sbx addresses its bridges from it - and also why a local
// proxy's fake-IP range tends to live there. Such a tool installs a host route over the whole
// block, and a bridge inside that route collides with it: the host has two ways to the same
// addresses, picks by prefix length, and whichever traffic takes the losing one fails with nothing
// to say why. So the host's table is read once at create and a routed slice is never asked for.
func TestABridgeIsNotAddressedOverAHostRoute(t *testing.T) {
	routes := []hostRoute{
		{prefix: netip.MustParsePrefix("198.18.0.0/16"), iface: "utun4"},
		{prefix: netip.MustParsePrefix("192.168.1.0/24"), iface: "en0"},
	}

	for subnet, want := range map[string]bool{
		"198.18.9.0/24":  true,  // inside the fake-IP range
		"198.18.0.0/16":  true,  // the route itself
		"198.19.44.0/24": false, // the other half of sbx's block, free
		"172.22.0.0/16":  false, // docker's own, nothing to do with this
	} {
		got := routedElsewhere(routes, netip.MustParsePrefix(subnet))
		if (got != "") != want {
			t.Errorf("routedElsewhere(%s) = %q, want a conflict: %v", subnet, got, want)
		}
	}

	// The operator can do exactly one thing about it, so the refusal has to name what to remove.
	where := routedElsewhere(routes, netip.MustParsePrefix("198.18.9.0/24"))
	if !strings.Contains(where, "198.18.0.0/16") || !strings.Contains(where, "utun4") {
		t.Fatalf("the conflict is reported as %q, which does not name the route or its interface", where)
	}

	err := routeConflict("osb-000000000001", []string{where})
	for _, want := range []string{"osb-000000000001", "utun4", egressSubnetBlock, "create the sandbox again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// The two tables sbx reads, in the shapes the platforms really print, and only the part of them
// this asks about: a route inside the egress block, named by its interface.
func TestTheHostsRoutesAreReadOnBothPlatforms(t *testing.T) {
	// `netstat -rn -f inet` on macOS: abbreviated destinations, a Netif column, and the host's
	// ordinary routes mixed in. The sandbox bridge sbx made is docker's, and on a Mac is not here
	// at all - the one on br-9f is the native-Linux shape, dropped because docker's overlap check
	// is what answers a taken slice.
	bsd := `Routing tables

Internet:
Destination        Gateway            Flags               Netif Expire
default            192.168.1.1        UGScg                 en0
127                127.0.0.1          UCS                   lo0
198.18/16          link#18            UCS                  utun4
198.19.7.1         link#18            UHLWIi               utun4
198.18.200.0/24    link#5             UC                    br-9f
224.0.0/4          link#18            UmCS                 utun4
`

	got := netstatRoutes([]byte(bsd))
	if len(got) != 2 {
		t.Fatalf("read %v from the BSD table, want the two routes inside %s that are not docker's",
			got, egressSubnetBlock)
	}

	if got[0].String() != "198.18.0.0/16 on utun4" || got[1].String() != "198.19.7.1/32 on utun4" {
		t.Errorf("the BSD table read as %v: an abbreviated prefix is the route it stands for, and "+
			"a bare address is a host route", got)
	}

	// /proc/net/route on Linux: little-endian hex of a network-order value, one line per route.
	// 198.18.0.0/16 is destination 0000.12C6 -> 0012C600 little-endian, mask 0000FFFF.
	linux := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"utun4\t000012C6\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"br-9f\t00C812C6\t00000000\t0001\t0\t0\t0\x0900FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"

	got = procNetRoutes([]byte(linux))
	if len(got) != 1 || got[0].String() != "198.18.0.0/16 on utun4" {
		t.Fatalf("read %v from /proc/net/route, want 198.18.0.0/16 on utun4 alone: the default "+
			"route is outside the block and br-9f is docker's own", got)
	}
}
