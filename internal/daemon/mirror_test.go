package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// shiftFor finds an offset that puts remote on a free local port. In production the local and
// remote numbers are equal because they are on different machines; here both ends share one
// loopback, so the mirror needs the same --port-offset a person would use.
func shiftFor(t *testing.T, remote int) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	return p - remote
}

func mirrorEcho(t *testing.T, port int) {
	t.Helper()

	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial local %d: %v", port, err)
	}
	defer c.Close()

	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through the mirror: %q %v", buf, err)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("never: %s", what)
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}

	_ = c.Close()

	return true
}

// The helper VM's daemon gains and loses sandboxes while the Mac's side is up. `sbx connect`
// binds once; the mirror has to follow, or a sandbox created after `sbx serve` started would
// never be reachable from the Mac.
func TestMirrorFollowsARemoteDaemon(t *testing.T) {
	p1, p2 := echoPort(t), echoPort(t)
	d := daemonFronting(p1, "i1")
	ts := serverFor(t, d)

	shift := shiftFor(t, p1)
	out := &syncBuffer{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- Mirror(ctx, MirrorOptions{
			Endpoint: Endpoint{URL: ts.URL, Token: testToken},
			Refresh:  50 * time.Millisecond,
			Shift:    shift,
			Out:      out,
		})
	}()

	eventually(t, "the first sandbox is mirrored", func() bool { return listening(p1 + shift) })
	mirrorEcho(t, p1+shift)

	// A sandbox created in the VM after the mirror started.
	d.mu.Lock()
	d.units["sbx-demo-cache"] = newUnit("demo", "cache", "sbx-demo-cache", "i2", "demo/cache",
		[]leg{{Listen: p2, Upstream: provider.Endpoint{Host: "127.0.0.1", Port: p2}}}, true)
	d.mu.Unlock()

	eventually(t, "a sandbox created later is mirrored", func() bool { return listening(p2 + shift) })
	mirrorEcho(t, p2+shift)

	// Removed in the VM: its local port must close, or it would accept and then fail every
	// connection, which reads as a broken database rather than a deleted one.
	d.mu.Lock()
	delete(d.units, "sbx-demo-db")
	d.mu.Unlock()

	eventually(t, "a removed sandbox's port closes", func() bool { return !listening(p1 + shift) })

	if !listening(p2 + shift) {
		t.Fatal("removing one sandbox closed another's port")
	}

	cancel()

	if err := <-done; err != nil {
		t.Fatalf("mirror returned %v after cancel", err)
	}

	if listening(p2 + shift) {
		t.Fatal("ports outlived the mirror")
	}

	for _, w := range []string{"demo/db", "demo/cache"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("the mirror never said it carried %s:\n%s", w, out.String())
		}
	}
}

// A restarted in-VM daemon issues new instance ids. The local port has to carry the new one,
// or every connection is refused as aimed at a unit that no longer exists.
func TestMirrorFollowsANewInstance(t *testing.T) {
	p := echoPort(t)
	d := daemonFronting(p, "old")
	ts := serverFor(t, d)
	shift := shiftFor(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = Mirror(ctx, MirrorOptions{Endpoint: Endpoint{URL: ts.URL, Token: testToken},
			Refresh: 50 * time.Millisecond, Shift: shift, Out: io.Discard})
	}()

	eventually(t, "mirrored", func() bool { return listening(p + shift) })

	d.mu.Lock()
	d.units["sbx-demo-db"].instance = "new"
	d.mu.Unlock()

	eventually(t, "the new instance is carried", func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p+shift), time.Second)
		if err != nil {
			return false
		}
		defer c.Close()

		_ = c.SetDeadline(time.Now().Add(time.Second))
		_, _ = c.Write([]byte("ping"))

		buf := make([]byte, 4)
		_, err = io.ReadFull(c, buf)

		return err == nil && string(buf) == "ping"
	})
}

// A remote that cannot be reached is a retry, not an exit: the in-VM daemon restarts under
// systemd, and the Mac's side should come back with it rather than need restarting too.
func TestMirrorRetriesAnUnreachableRemote(t *testing.T) {
	out := &syncBuffer{}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := Mirror(ctx, MirrorOptions{Endpoint: Endpoint{URL: "http://127.0.0.1:1", Token: testToken},
		Refresh: 50 * time.Millisecond, Out: out})
	if err != nil {
		t.Fatalf("an unreachable remote ended the mirror: %v", err)
	}

	if n := strings.Count(out.String(), "could not reach"); n != 1 {
		t.Fatalf("said so %d times; once per outage, not once per tick:\n%s", n, out.String())
	}
}

func TestMirrorRefusesARejectedToken(t *testing.T) {
	ts := serverFor(t, daemonFronting(echoPort(t), "i1"))

	err := Mirror(context.Background(), MirrorOptions{Endpoint: Endpoint{URL: ts.URL, Token: "wrong"},
		Refresh: 50 * time.Millisecond, Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "rejected the token") {
		t.Fatalf("a wrong token must end the mirror, not retry forever: %v", err)
	}
}

// Connections are accepted and carried while the mirror keeps refreshing the source they belong
// to: the refresh rewrites the source's half-close flag, and every tunnel reads it. Run under
// -race, this is the case that tripped the detector (tunnelOne reading, Mirror writing).
func TestMirrorRefreshesWhileConnectionsRun(t *testing.T) {
	p := echoPort(t)
	d := daemonFronting(p, "i1")
	ts := serverFor(t, d)
	shift := shiftFor(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = Mirror(ctx, MirrorOptions{Endpoint: Endpoint{URL: ts.URL, Token: testToken},
			Refresh: time.Millisecond, Shift: shift, Out: io.Discard})
	}()

	eventually(t, "mirrored", func() bool { return listening(p + shift) })

	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 10 {
				mirrorEcho(t, p+shift)
			}
		}()
	}

	wg.Wait()
}

// A Sync is answered once the pass it asked for is over - even when that pass could not reach the
// fleet - so a caller waiting on it (the helper VM's API front) is never held past one fetch.
func TestMirrorSyncIsAnsweredWhenTheRemoteIsUnreachable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kick := make(chan chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- Mirror(ctx, MirrorOptions{Endpoint: Endpoint{URL: "http://127.0.0.1:1", Token: testToken},
			Refresh: time.Hour, Out: io.Discard, Sync: kick})
	}()

	answered := make(chan struct{})

	select {
	case kick <- answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the mirror never took the sync request")
	}

	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("a sync against an unreachable remote was never answered")
	}

	cancel()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
