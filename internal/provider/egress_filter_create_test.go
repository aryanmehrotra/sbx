package provider

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// colimaDocker is a `docker` on PATH shaped like colima: the sandbox's gateway is inside the VM,
// so this machine cannot bind it and the filter has to run as a container. It logs every call,
// one per line, and answers what Create and ensureFilterContainer ask.
//
// serviceExists makes the service's container already exist - `sbx create` run again over a
// sandbox - and filterLabels is what the running filter container reports for
// ensureFilterContainer's inspect, or "" for no filter container at all.
func colimaDocker(t *testing.T, serviceExists bool, filterLabels string) (calls string) {
	t.Helper()

	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")

	svc, svcFormat := "exit 1", "exit 1"
	if serviceExists {
		// Create asks the existing container for its image and tier, and recreates it when the
		// image differs from the spec's. Every spec here declares alpine:3.20, so the container
		// reports that - the image is unchanged, and only the filter is under test.
		svc, svcFormat = "exit 0", "echo 'alpine:3.20|container'; exit 0"
	}

	filter := "exit 1"
	if filterLabels != "" {
		filter = "printf '%s\\n' '" + filterLabels + "'; exit 0"
	}

	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + calls + "\n" +
		"case \"$*\" in\n" +
		"  'network ls -q') echo n1; echo n2; exit 0 ;;\n" +
		"  *'network inspect --format'*Gateway*' n1 n2') echo '172.17.0.1 172.17.0.0/16 '; echo '172.30.0.1 '; exit 0 ;;\n" +
		"  *'network inspect sbx-noegress-'*Subnet*) echo '172.30.0.0/16'; exit 0 ;;\n" +
		"  *'network inspect sbx-noegress-'*Gateway*) echo '192.0.2.1'; exit 0 ;;\n" +
		"  'network inspect '*) exit 0 ;;\n" +
		"  'inspect --format'*sbx-egressfilter-*) " + filter + " ;;\n" +
		"  'inspect --format'*' sbx-'*) " + svcFormat + " ;;\n" +
		"  'inspect sbx-'*) " + svc + " ;;\n" +
		"  'image inspect '*) exit 0 ;;\n" +
		"  'port '*) echo '127.0.0.1:41234'; exit 0 ;;\n" +
		"  'run '*) echo deadbeef; exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"

	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return calls
}

func callLog(t *testing.T, path string) []string {
	t.Helper()

	b, _ := os.ReadFile(path)

	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func calledWith(calls []string, prefix string) (string, bool) {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return c, true
		}
	}

	return "", false
}

// Running `sbx create` again over a sandbox whose spec's egress changed used to keep the old filter
// silently: the filter was only ensured when a service container was created, so for a service
// that already existed nothing asked, and a host just removed from the spec stayed reachable.
func TestCreateOverAnExistingSandboxReplacesAFilterWhoseDeclarationChanged(t *testing.T) {
	old := `{"defaultAction":"deny","egress":[{"action":"allow","target":"old.example"},{"action":"allow","target":"*.old.example"}]}`
	calls := colimaDocker(t, true, old+"\x1f\x1f172.30.255.254")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, EgressAllow: []string{"new.example"}}

	if err := d.Create(context.Background(), "fx", 3, 0, "app", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("create: %v", err)
	}

	log := callLog(t, calls)

	if _, ok := calledWith(log, "rm -f sbx-egressfilter-fx"); !ok {
		t.Fatalf("the filter enforcing the old declaration was kept:\n%s", strings.Join(log, "\n"))
	}

	run, ok := calledWith(log, "run -d --name sbx-egressfilter-fx")
	if !ok || !strings.Contains(run, "new.example") || strings.Contains(run, "old.example") {
		t.Fatalf("no filter was started with the new declaration:\n%s", strings.Join(log, "\n"))
	}

	if _, ok := calledWith(log, "run -d --name sbx-fx-app"); ok {
		t.Fatal("the existing service container was created again")
	}
}

// The same create with an unchanged declaration keeps the filter, and with it every tunnel open
// through it and every change made live with sbx egress.
func TestCreateOverAnExistingSandboxKeepsAnUnchangedFilter(t *testing.T) {
	withGitHub := `{"defaultAction":"deny","egress":[{"action":"allow","target":"new.example"},{"action":"allow","target":"*.new.example"},{"action":"allow","target":"github.com"},{"action":"allow","target":"*.github.com"}]}`
	tag, err := filterImageWant()
	if err != nil {
		t.Fatal(err)
	}

	calls := colimaDocker(t, true, withGitHub+"\x1fgithub.com:22\x1f172.30.255.254\x1f"+tag)

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, EgressAllow: []string{"new.example", "github.com:22"}}

	if err := d.Create(context.Background(), "fx", 3, 0, "app", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("create: %v", err)
	}

	log := callLog(t, calls)
	if c, ok := calledWith(log, "rm -f sbx-egressfilter-fx"); ok {
		t.Fatalf("an unchanged filter was replaced (%s):\n%s", c, strings.Join(log, "\n"))
	}
}

// A changed port grant is a changed declaration too: the grants are the filter's -ports argument,
// fixed for its life.
func TestAChangedPortGrantReplacesTheFilter(t *testing.T) {
	same := `{"defaultAction":"deny","egress":[{"action":"allow","target":"new.example"},{"action":"allow","target":"*.new.example"}]}`
	calls := colimaDocker(t, true, same+"\x1f\x1f172.30.255.254")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, EgressAllow: []string{"new.example:2222"}}

	if err := d.Create(context.Background(), "fx", 3, 0, "app", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("create: %v", err)
	}

	log := callLog(t, calls)

	run, ok := calledWith(log, "run -d --name sbx-egressfilter-fx")
	if !ok || !strings.Contains(run, "-ports new.example:2222") {
		t.Fatalf("the filter was not started with the new port grant:\n%s", strings.Join(log, "\n"))
	}
}

// A new filter container: fixed address on the sandbox's bridge, the engine's gateways and the
// default bridge refused, and the service told where it is by /etc/hosts as well as by DNS -
// gVisor's netstack does not use docker's embedded DNS, so under --isolation gvisor the alias
// alone left HTTP_PROXY pointing at a name that never resolved.
func TestANewFilterContainerIsAddressedAndClosedOffTheEngine(t *testing.T) {
	calls := colimaDocker(t, false, "")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, Egress: spec.EgressAllow}

	if err := d.Create(context.Background(), "fx", 3, 0, "app", svc,
		[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("create: %v", err)
	}

	log := callLog(t, calls)

	filter, ok := calledWith(log, "run -d --name sbx-egressfilter-fx")
	if !ok {
		t.Fatalf("no filter container:\n%s", strings.Join(log, "\n"))
	}

	for _, want := range []string{"--ip 172.30.255.254", "-refuse 172.17.0.1,172.17.0.0/16,172.30.0.1,192.0.2.1"} {
		if !strings.Contains(filter, want) {
			t.Errorf("the filter was started without %q:\n%s", want, filter)
		}
	}

	service, ok := calledWith(log, "run -d --name sbx-fx-app")
	if !ok || !strings.Contains(service, "--add-host sbx-egress:172.30.255.254") {
		t.Errorf("the service cannot find its filter without docker's DNS:\n%s", service)
	}
}

// After `sbx create` replaces a filter for a changed spec, the services that already existed keep
// labels naming the old declaration. The filter's label is the one that counts: a reset returns
// to it, and a live copy saved against the old one is recognised as stale and dropped.
func TestTheFilterContainersDeclarationWinsOverStaleServiceLabels(t *testing.T) {
	old := `{"defaultAction":"deny","egress":[{"action":"allow","target":"old.example"}]}`
	f, err := filterOf("fx", []Unit{{Sandbox: "fx", Service: "app", EgressGateway: "172.30.0.1", EgressPolicy: old}})
	if err != nil {
		t.Fatal(err)
	}

	now := `{"defaultAction":"deny","egress":[{"action":"allow","target":"new.example"}]}`

	got := filterDeclaration(f.Declared, now)
	if len(got.Egress) != 1 || got.Egress[0].Target != "new.example" {
		t.Fatalf("declared = %+v, want the filter's new.example", got)
	}

	if got := filterDeclaration(f.Declared, ""); got.Egress[0].Target != "old.example" {
		t.Fatalf("a filter with no label lost the services' declaration: %+v", got)
	}
}

// A re-run create that replaces the egress filter makes the filter the sandbox's newest
// container. `docker ps` lists newest first, the filter carries no slot, and the sandbox's slot
// was then looked up on it, missed, and a new one handed out.
func TestTheSlotIsNotReadOffTheEgressFilter(t *testing.T) {
	dir := t.TempDir()

	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  'ps -aq --filter label=sbx.sandbox=fx --filter label=sbx.slot') echo svc ;;\n" +
		"  'ps -aq --filter label=sbx.sandbox=fx') echo filter; echo svc ;;\n" +
		"  'inspect svc '*) echo 3 ;;\n" +
		"  'inspect filter '*) echo ;;\n" +
		"esac\n"

	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	if slot, ok := d.slotOf("fx"); !ok || slot != 3 {
		t.Fatalf("slotOf = %d, %v; want the service's slot 3", slot, ok)
	}
}

// A microVM's filter has no port grants, so a host:port entry is refused there by name rather than
// accepted and quietly limited to 80 and 443.
func TestFirecrackerRefusesAnAllowEntryWithAPort(t *testing.T) {
	if err := unsupported(spec.Service{Image: "x", EgressAllow: []string{"github.com:22"}}); err == nil ||
		!strings.Contains(err.Error(), "host:port") {
		t.Fatalf("a port grant on firecracker = %v, want a refusal naming it", err)
	}

	if err := unsupported(spec.Service{Image: "x", EgressAllow: []string{"github.com", "pypi.org:443"}}); err != nil {
		t.Fatalf("an allow-list with no extra port was refused: %v", err)
	}
}

// A filter built by an older sbx keeps running the old filter code - a fix to the filter never
// reached a sandbox created before it. A re-run create replaces it. The declaration is unchanged,
// so the new filter carries the same policy label and EgressControl.Sync pushes any live change
// back (TestSyncRestoresTheLivePolicyToAReplacedFilter): live changes are kept, and the message
// does not claim they were dropped.
func TestAFilterBuiltByAnOlderSbxIsReplacedKeepingItsDeclaration(t *testing.T) {
	same := `{"defaultAction":"deny","egress":[{"action":"allow","target":"new.example"},{"action":"allow","target":"*.new.example"}]}`
	calls := colimaDocker(t, true, same+"\x1f\x1f172.30.255.254\x1fsbx-egress-filter:0123456789abcdef")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	svc := spec.Service{Image: "alpine:3.20", Ports: []int{8080}, EgressAllow: []string{"new.example"}}

	out := captureStdout(t, func() {
		if err := d.Create(context.Background(), "fx", 3, 0, "app", svc,
			[]Endpoint{{Host: "127.0.0.1", Port: 20060}}, t.TempDir(), IsolationContainer); err != nil {
			t.Fatalf("create: %v", err)
		}
	})

	log := callLog(t, calls)

	if _, ok := calledWith(log, "rm -f sbx-egressfilter-fx"); !ok {
		t.Fatalf("a filter built by an older sbx was kept:\n%s", strings.Join(log, "\n"))
	}

	if !strings.Contains(out, "replacing the egress filter for fx: built by an older sbx") ||
		strings.Contains(out, "dropped") {
		t.Errorf("the replacement did not say why, or claimed live changes were dropped: %q", out)
	}

	run, ok := calledWith(log, "run -d --name sbx-egressfilter-fx")
	if !ok || !strings.Contains(run, "--label sbx.egress.policy="+same+" ") {
		t.Fatalf("the replacement changed the declaration, so Sync would drop live changes:\n%s", run)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	old := os.Stdout
	os.Stdout = w

	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	f()

	os.Stdout = old
	_ = w.Close()

	return <-done
}

// The daemon's view of the engine's doors, pushed to every running filter each tick: every
// network's gateway and the default bridge's subnet, with nothing sandbox-specific in it - the
// same list whichever filter it goes to.
func TestEgressDoorsIsEveryGatewayAndTheDefaultBridge(t *testing.T) {
	colimaDocker(t, false, "")

	d := newDocker(dockerEndpoint{Network: "unix", Address: "/var/run/docker.sock"})

	got, err := d.EgressDoors(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(got, ",") != "172.17.0.1,172.17.0.0/16,172.30.0.1" {
		t.Fatalf("EgressDoors = %v", got)
	}
}
