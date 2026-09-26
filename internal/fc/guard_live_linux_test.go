package fc

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The guard against the real kernel, on a real bridge: SBX_GUARD_LIVE=1, as root, on Linux. It
// builds docker's shape without docker - a nat PREROUTING DNAT for local addresses onto a
// "container" in another namespace, and a FORWARD accept for it - puts a "guest" namespace on an
// sbxfc bridge, and checks from inside the guest what it can reach before and after Install:
//
//	before: the host service and the DNAT'd port both answer (the bypass H1 was)
//	after:  both time out; the filter port still answers; the host still reaches the guest
//
// Everything it makes is named from slot 250 and removed at the end.
func TestGuardLive(t *testing.T) {
	if os.Getenv("SBX_GUARD_LIVE") != "1" || os.Geteuid() != 0 {
		t.Skip("SBX_GUARD_LIVE=1 as root on Linux: makes a bridge, namespaces and iptables rules")
	}

	const (
		slot     = 250
		dnatPort = "18080" // a "docker-published" port, DNAT'd onto the container namespace
		hostPort = "18081" // a host service bound to 0.0.0.0
		filter   = 20999
		guestSvc = "18082" // a service in the guest the host dials (the wake proxy's direction)
	)

	a := Addr{Slot: slot}
	gw, guestIP := a.Gateway(), "10.231.250.2"

	sh := func(args ...string) string {
		t.Helper()

		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
		}

		return string(out)
	}

	try := func(args ...string) { _ = exec.Command(args[0], args[1:]...).Run() }

	cleanup := func() {
		_ = NewGuard(filter).Release(context.Background(), a)
		try("iptables", "-t", "nat", "-D", "PREROUTING", "-m", "addrtype", "--dst-type", "LOCAL", "-p", "tcp",
			"--dport", dnatPort, "-j", "DNAT", "--to-destination", "10.99.250.2:"+dnatPort)
		try("iptables", "-D", "FORWARD", "-d", "10.99.250.2", "-p", "tcp", "--dport", dnatPort, "-j", "ACCEPT")
		try("iptables", "-D", "FORWARD", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
		try("ip", "netns", "del", "sbxg250")
		try("ip", "netns", "del", "sbxt250")
		try("ip", "link", "del", "vg250")
		try("ip", "link", "del", "vt250")
		try("ip", "link", "del", a.Bridge())
	}

	cleanup()
	t.Cleanup(cleanup)

	// The bridge, and a guest on it.
	sh("ip", "link", "add", a.Bridge(), "type", "bridge")
	sh("ip", "addr", "add", gw+"/24", "dev", a.Bridge())
	sh("ip", "link", "set", a.Bridge(), "up")
	sh("ip", "netns", "add", "sbxg250")
	sh("ip", "link", "add", "vg250", "type", "veth", "peer", "name", "eth0", "netns", "sbxg250")
	sh("ip", "link", "set", "vg250", "master", a.Bridge(), "up")
	sh("ip", "netns", "exec", "sbxg250", "ip", "addr", "add", guestIP+"/24", "dev", "eth0")
	sh("ip", "netns", "exec", "sbxg250", "ip", "link", "set", "eth0", "up")
	sh("ip", "netns", "exec", "sbxg250", "ip", "link", "set", "lo", "up")
	sh("ip", "netns", "exec", "sbxg250", "ip", "route", "add", "default", "via", gw)

	// The "container": another namespace, reached by routing, as docker's bridge is.
	sh("ip", "netns", "add", "sbxt250")
	sh("ip", "link", "add", "vt250", "type", "veth", "peer", "name", "eth0", "netns", "sbxt250")
	sh("ip", "addr", "add", "10.99.250.1/24", "dev", "vt250")
	sh("ip", "link", "set", "vt250", "up")
	sh("ip", "netns", "exec", "sbxt250", "ip", "addr", "add", "10.99.250.2/24", "dev", "eth0")
	sh("ip", "netns", "exec", "sbxt250", "ip", "link", "set", "eth0", "up")
	sh("ip", "netns", "exec", "sbxt250", "ip", "route", "add", "default", "via", "10.99.250.1")

	// Docker's shape: DNAT for any local address on the published port, FORWARD accepts it.
	sh("sysctl", "-qw", "net.ipv4.ip_forward=1")
	sh("iptables", "-t", "nat", "-A", "PREROUTING", "-m", "addrtype", "--dst-type", "LOCAL", "-p", "tcp",
		"--dport", dnatPort, "-j", "DNAT", "--to-destination", "10.99.250.2:"+dnatPort)
	sh("iptables", "-I", "FORWARD", "1", "-d", "10.99.250.2", "-p", "tcp", "--dport", dnatPort, "-j", "ACCEPT")
	sh("iptables", "-I", "FORWARD", "1", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Listeners: the container's, the host's, the filter's, and the guest's own.
	listen := func(ns, addr string) {
		cmd := exec.Command("ip", "netns", "exec", ns, self, "-test.run", "^TestGuardLiveHelper$")
		cmd.Env = append(os.Environ(), "SBX_GUARD_HELPER=listen", "SBX_GUARD_ADDR="+addr)

		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	}

	listen("sbxt250", "0.0.0.0:"+dnatPort)
	listen("sbxg250", "0.0.0.0:"+guestSvc)

	for _, addr := range []string{"0.0.0.0:" + hostPort, fmt.Sprintf("%s:%d", gw, filter)} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { ln.Close() })

		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}

				c.Close()
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)

	fromGuest := func(addr string) bool {
		cmd := exec.Command("ip", "netns", "exec", "sbxg250", self, "-test.run", "^TestGuardLiveHelper$")
		cmd.Env = append(os.Environ(), "SBX_GUARD_HELPER=dial", "SBX_GUARD_ADDR="+addr)

		return cmd.Run() == nil
	}

	fromHost := func(addr string) bool {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			c.Close()
		}

		return err == nil
	}

	published, hostSvc := gw+":"+dnatPort, gw+":"+hostPort
	filterAddr, guest := fmt.Sprintf("%s:%d", gw, filter), guestIP+":"+guestSvc

	// Before: the rig reproduces the bypass, or the test proves nothing.
	for addr, want := range map[string]bool{published: true, hostSvc: true, filterAddr: true} {
		if got := fromGuest(addr); got != want {
			t.Fatalf("before the guard, guest -> %s = %v, want %v (the rig does not reproduce docker's shape)", addr, got, want)
		}
	}

	if err := NewGuard(filter).Install(context.Background(), a); err != nil {
		t.Fatalf("Install on a real kernel: %v", err)
	}

	for addr, want := range map[string]bool{published: false, hostSvc: false, filterAddr: true} {
		if got := fromGuest(addr); got != want {
			t.Errorf("guarded, guest -> %s = %v, want %v", addr, got, want)
		}
	}

	if !fromHost(guest) {
		t.Error("guarded, the host can no longer reach its guest (the wake proxy's direction)")
	}

	// Idempotent against the real iptables, and Whole agrees.
	g := NewGuard(filter)
	if repaired, err := g.Ensure(context.Background(), a); err != nil || repaired {
		t.Errorf("Ensure on a whole guard = %v, %v", repaired, err)
	}

	sh("iptables", "-t", "mangle", "-D", "PREROUTING", "-i", a.Bridge(), "-j", a.Chain())

	if whole, _ := g.Whole(context.Background(), a); whole {
		t.Error("Whole missed a removed hook")
	}

	if repaired, err := g.Ensure(context.Background(), a); err != nil || !repaired || fromGuest(published) {
		t.Errorf("Ensure after a removed hook = %v, %v, and the port is reachable again: %v", repaired, err, fromGuest(published))
	}

	t.Logf("real iptables rules:\n%s%s", sh("iptables", "-t", "mangle", "-S"), sh("iptables", "-S", a.Chain()))
}

// TestGuardLiveHelper is TestGuardLive's hands inside a namespace: it listens, or dials once.
func TestGuardLiveHelper(t *testing.T) {
	addr := os.Getenv("SBX_GUARD_ADDR")

	switch os.Getenv("SBX_GUARD_HELPER") {
	case "listen":
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}

		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			c.Close()
		}
	case "dial":
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}

		c.Close()
	default:
		t.Skip("run by TestGuardLive")
	}
}
