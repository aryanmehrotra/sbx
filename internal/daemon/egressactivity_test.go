package daemon

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The gap this closes: a sandbox running an agent takes no inbound connection, so on the bytes
// sbx measures it looks idle from the moment it starts working. Its API calls are the one thing
// sbx can still see, and these tests are about seeing them.

func TestForwardStampsActivity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello from the api")
	}))
	defer upstream.Close()

	host := strings.TrimPrefix(upstream.URL, "http://")

	var n atomic.Int64

	f := NewEgressFilter([]string{host})
	f.OnActivity = func() { n.Add(1) }

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := (&http.Transport{Proxy: fixedProxy(t, proxy.URL)}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if got := string(body); got != "hello from the api" {
		t.Fatalf("body = %q, want the upstream's", got)
	}

	if n.Load() == 0 {
		t.Fatal("a permitted request through the filter stamped no activity; a box that only " +
			"calls out would still look idle")
	}
}

func TestForwardDeniedStampsNothing(t *testing.T) {
	var n atomic.Int64

	f := NewEgressFilter([]string{"allowed.example"})
	f.OnActivity = func() { n.Add(1) }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://blocked.example/x", nil)
	req.Host = "blocked.example"

	f.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}

	if n.Load() != 0 {
		t.Fatal("a refused request stamped activity: a box hammering a blocked host would " +
			"hold its memory forever without ever reaching anything")
	}
}

func TestTunnelStampsActivityAsBytesMove(t *testing.T) {
	// An upstream that dribbles, standing in for a streaming API response: if the filter only
	// stamped when the CONNECT opened, the idle timer could fire in the middle of this.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()

		for range 5 {
			if _, err := c.Write([]byte("chunk")); err != nil {
				return
			}

			time.Sleep(2 * time.Millisecond)
		}
	}()

	host := ln.Addr().String()

	var n atomic.Int64

	f := NewEgressFilter([]string{"127.0.0.1"})
	f.OnActivity = func() { n.Add(1) }

	proxy := httptest.NewServer(f)
	defer proxy.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("CONNECT " + host + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 4096)

	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	total := 0
	for total < len("chunk")*5 {
		k, err := conn.Read(buf)
		if err != nil {
			break
		}

		total += k
	}

	// Once for admission, then again as the streamed bytes came back. More than one is the
	// whole point: a stamp per open would not keep a long response alive.
	if n.Load() < 2 {
		t.Fatalf("stamps = %d, want more than one; a streaming response must keep stamping", n.Load())
	}
}

func TestFilterWithoutHookIsUnchanged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	host := strings.TrimPrefix(upstream.URL, "http://")

	f := NewEgressFilter([]string{host}) // no OnActivity: every existing caller
	proxy := httptest.NewServer(f)

	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)

	resp, err := (&http.Transport{Proxy: fixedProxy(t, proxy.URL)}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q; a nil hook must not change what the filter carries", body)
	}
}

func TestDueThrottlesToOneWalkASecond(t *testing.T) {
	p := &egressProxy{}
	now := time.Now().UnixNano()

	if !p.due(now) {
		t.Fatal("the first stamp must go through")
	}

	if p.due(now + int64(100*time.Millisecond)) {
		t.Fatal("a second stamp 100ms later must be skipped: a download would otherwise take " +
			"the daemon lock once per chunk")
	}

	if !p.due(now + int64(2*time.Second)) {
		t.Fatal("a stamp two seconds later must go through")
	}
}

func TestDueIsRaceFreeAndAdmitsOne(t *testing.T) {
	p := &egressProxy{}
	now := time.Now().UnixNano()

	var (
		wg     sync.WaitGroup
		passed atomic.Int64
	)

	for range 64 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if p.due(now) {
				passed.Add(1)
			}
		}()
	}

	wg.Wait()

	if got := passed.Load(); got != 1 {
		t.Fatalf("%d of 64 concurrent stamps passed, want exactly 1", got)
	}
}
