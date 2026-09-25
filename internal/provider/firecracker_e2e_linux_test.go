package provider

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// TestFirecrackerE2E boots a real VM: create (rootfs, cold boot, first port, snapshot) → start
// (snapshot/load, resume) → first byte → stop (Diff, merged) → start again → first byte → rm.
//
// Opt-in and Linux-only: SBX_FC_E2E=1 on a host with a usable /dev/kvm, e2fsprogs, iproute2,
// CAP_NET_ADMIN (in practice root) and a docker engine holding or able to pull the image. It
// downloads the pinned firecracker and kernel on first run. SBX_FC_E2E_IMAGE overrides the image
// (default redis:7-alpine, which answers PING with +PONG, a first byte with no client library).
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
