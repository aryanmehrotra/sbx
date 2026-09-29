package cli

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// unitsStub lists fixed units; nothing else of the provider is used.
type unitsStub struct {
	provider.Provider

	units []provider.Unit
}

func (u *unitsStub) List(context.Context, string) ([]provider.Unit, error) { return u.units, nil }

// acceptThenClose is a workload that no client can talk to: it takes the connection and drops it,
// which is what docker-proxy does for a container whose guest has no network.
func acceptThenClose(t *testing.T) provider.Endpoint {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			_ = c.Close()
		}
	}()

	return provider.Endpoint{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
}

func refusedEndpoint(t *testing.T) provider.Endpoint {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	return provider.Endpoint{Host: "127.0.0.1", Port: port}
}

// Create printed "ready" for a sandbox whose workload no connection could reach: the health check
// runs inside the container, so it passes there. Create now asks the same question sbx ready does,
// for the services it just made that are running.
func TestCreateRefusesToCallAnUnreachableWorkloadReady(t *testing.T) {
	p := &unitsStub{units: []provider.Unit{
		{Sandbox: "x", Service: "bad", Running: true, Upstream: []provider.Endpoint{acceptThenClose(t)},
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
	}}

	err := checkCreatedWorkloads(context.Background(), p, "x", []string{"bad"}, time.Time{}, time.Now().Add(500*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("a workload that closes every connection got %v, want it named as not serving", err)
	}
}

// A service that is asleep after its create - a microVM's create ends by snapshotting it - is not
// dialled: its port has nothing behind it until a connection wakes it, and that is not a failure.
// Nor is a service this create did not make.
func TestCreateDoesNotDialAnAsleepOrUntouchedService(t *testing.T) {
	p := &unitsStub{units: []provider.Unit{
		{Sandbox: "x", Service: "vm", Running: false, Upstream: []provider.Endpoint{refusedEndpoint(t)},
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
		{Sandbox: "x", Service: "other", Running: true, Upstream: []provider.Endpoint{acceptThenClose(t)},
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 1}}},
	}}

	if err := checkCreatedWorkloads(context.Background(), p, "x", []string{"vm"}, time.Time{}, time.Now().Add(300*time.Millisecond)); err != nil {
		t.Fatalf("an asleep service and one this create did not make were checked: %v", err)
	}
}
