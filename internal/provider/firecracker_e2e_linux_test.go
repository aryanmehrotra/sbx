package provider

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// TestFirecrackerE2E boots a real VM: create (rootfs, cold boot, first port, snapshot) → start
// (snapshot/load, resume) → first byte → stop (Diff, merged) → start again → first byte → rm.
//
// Opt-in and Linux-only: SBX_FC_E2E=1 on a host with a usable /dev/kvm, e2fsprogs, iproute2,
// CAP_NET_ADMIN (in practice root) and a docker engine holding or able to pull the image. It
// downloads the pinned firecracker and kernel on first run. SBX_FC_E2E_IMAGE overrides the image
// (default redis:7-alpine, which answers PING with +PONG, a first byte with no client library).
// Run as a compiled test binary, set SBX_EXECD_BINARY to a real linux sbx: the agent drive is
// otherwise built from os.Executable(), which is then the test binary, and PID 1 exits at once.
//
// It reports timings with t.Logf and asserts only correctness: the numbers are for reading,
// and one run on one host is not a benchmark.
func TestFirecrackerE2E(t *testing.T) {
	if os.Getenv("SBX_FC_E2E") != "1" {
		t.Skip("SBX_FC_E2E=1 to boot a real Firecracker VM")
	}

	image := os.Getenv("SBX_FC_E2E_IMAGE")
	if image == "" {
		image = "redis:7-alpine"
	}

	prov, err := For("firecracker", "", "")
	if err != nil {
		t.Fatal(err)
	}

	p := prov.(*fcProvider)
	ctx := context.Background()
	sandbox := fmt.Sprintf("fce2e%d", time.Now().UnixNano()%100000)

	slot, err := p.AllocSlot(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}

	svc := spec.Service{Image: image, Ports: []int{6379}}
	eps := p.Endpoints(sandbox, "cache", slot, 0, svc.Ports)

	t.Cleanup(func() { _ = p.Remove(context.WithoutCancel(ctx), sandbox) })

	start := time.Now()
	if err := p.Create(ctx, sandbox, slot, 0, "cache", svc, eps, "", IsolationContainer); err != nil {
		t.Fatal(err)
	}

	t.Logf("create (rootfs, cold boot, serve, snapshot): %s", time.Since(start))

	ref := containerName(sandbox, "cache")
	vm, _ := p.load(ref)
	t.Logf("rootfs clone: %s", vm.Clone)

	for round := 1; round <= 2; round++ {
		start = time.Now()
		if err := p.Start(ctx, ref); err != nil {
			t.Fatalf("round %d start: %v\n%s", round, err, p.consoleTail(p.dir(ref), 30))
		}

		loaded := time.Since(start)

		if got := ping(t, vm.addr().GuestIP(), 6379); got != "+PONG" {
			t.Fatalf("round %d: %q\n%s", round, got, p.consoleTail(p.dir(ref), 30))
		}

		t.Logf("round %d: start %s, first byte %s", round, loaded, time.Since(start))

		assertJailed(t, p, vm)

		// Through execd over the vsock device, on a VM that was just restored and re-keyed: a
		// stale token, or a Seal/Rekey secret mismatch, fails here or at the Stop below.
		start = time.Now()

		out, err := p.Exec(ctx, ref, []string{"redis-cli", "ping"})
		if err != nil || out != "PONG" {
			t.Fatalf("round %d exec over vsock: %q, %v\n%s", round, out, err, p.consoleTail(p.dir(ref), 30))
		}

		t.Logf("round %d: exec over vsock %s", round, time.Since(start))

		if vm, _ = p.load(ref); vm.Generation != uint64(round) || vm.LiveSecret == "" || vm.LiveSecret == vm.ControlSecret {
			t.Fatalf("round %d: generation %d, live secret rotated %v", round, vm.Generation, vm.LiveSecret != vm.ControlSecret)
		}

		start = time.Now()
		if err := p.Stop(ctx, ref); err != nil {
			t.Fatalf("round %d stop: %v", round, err)
		}

		t.Logf("round %d: stop (snapshot) %s", round, time.Since(start))

		if units, _ := p.List(ctx, sandbox); units[0].Running {
			t.Fatalf("round %d: still running after Stop", round)
		}
	}
}

// assertJailed checks, from /proc, that the running VMM is the jailer's work: its own uid and gid
// throughout (never 0), no capabilities, a root that is its jail (the VM's disk is there, the
// host's /etc is not) and its own cgroup. Unless SBX_FC_JAILER=off, which says so in the log.
func assertJailed(t *testing.T, p *fcProvider, vm *fcVM) {
	t.Helper()

	if p.jail == nil {
		t.Logf("%s=off: the VMM runs unjailed, as root", fc.JailerEnv)
		return
	}

	dir := p.dir(vm.Ref)

	pid, err := fc.ReadPID(dir)
	if err != nil {
		t.Fatal(err)
	}

	proc := "/proc/" + strconv.Itoa(pid)
	want := strconv.Itoa(p.jail.UID(vm.addr()))

	status, _ := os.ReadFile(proc + "/status")
	for _, l := range strings.Split(string(status), "\n") {
		f := strings.Fields(l)

		switch {
		case len(f) == 5 && (f[0] == "Uid:" || f[0] == "Gid:"):
			for _, id := range f[1:] {
				if id != want || id == "0" {
					t.Fatalf("VMM %s, want %s throughout", l, want)
				}
			}
		case len(f) == 2 && f[0] == "CapEff:" && f[1] != "0000000000000000":
			t.Fatalf("the VMM kept capabilities: %s", l)
		}
	}

	if _, err := os.Stat(proc + "/root/etc/passwd"); err == nil {
		t.Fatal("the VMM sees the host's /etc: it is not chrooted")
	}

	a, _ := os.Stat(dir + "/" + fc.RootfsName)
	j, _ := os.Stat(proc + "/root/" + fc.RootfsName)

	if a == nil || j == nil || !os.SameFile(a, j) {
		t.Fatal("the jailed VMM's /rootfs.ext4 is not the VM's disk")
	}

	if cg, _ := os.ReadFile(proc + "/cgroup"); !strings.Contains(string(cg), "/"+fc.JailCgroupParent+"/"+fc.JailID(dir)) {
		t.Fatalf("the VMM's cgroup is %q", cg)
	}

	t.Logf("jailed: pid %d uid %s, chrooted in %s", pid, want, fc.JailRoot(dir, vm.Binary))
}

func ping(t *testing.T, host string, port int) string {
	t.Helper()

	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := c.Write([]byte("PING\r\n")); err != nil {
		t.Fatal(err)
	}

	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}

	return line[:len(line)-2]
}
