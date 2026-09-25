//go:build unix

package execd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/execdctl"
	"github.com/aryanmehrotra/sbx/internal/fcvsock"
)

const (
	bootSecret = "boot-secret-0123456789abcdef0123456789"
	nextSecret = "next-secret-0123456789abcdef0123456789"
)

// ctl is the host's client for a test server, dialling it over TCP where a real host would dial
// the vsock device.
func (s *testServer) ctl() execdctl.Client {
	addr := strings.TrimPrefix(s.ts.URL, "http://")

	return execdctl.Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}}
}

func code(err error) string {
	var e *execdctl.Error
	if errors.As(err, &e) {
		return e.Code
	}

	return fmt.Sprintf("not an execd refusal: %v", err)
}

// runEcho runs `echo <expr>` with tok and returns the status and what it printed.
func (s *testServer) runEcho(tok, expr string) (int, string) {
	s.t.Helper()

	st, _, data := s.do("POST", "/command", map[string]any{"command": "echo " + expr}, AccessTokenHeader, tok)
	if st != http.StatusOK {
		return st, string(data)
	}

	ex, err := parseExecution(bytes.NewReader(data))
	if err != nil {
		s.t.Fatal(err)
	}

	return st, strings.TrimSpace(ex.Text())
}

// A restore's re-key replaces the token - the snapshot's token stops working in this clone at
// once - and the control secret, so the secret every clone inherited cannot re-key it again.
func TestRekeyReplacesTokenAndSecret(t *testing.T) {
	t.Setenv("SBX_REKEY_VAR", "")
	os.Unsetenv("SBX_REKEY_VAR")

	s := newTestServer(t, Options{AccessToken: "parent", ControlSecret: bootSecret})
	ctx := context.Background()

	if st, _ := s.runEcho("parent", "hi"); st != http.StatusOK {
		t.Fatalf("parent token before re-key: %d", st)
	}

	err := s.ctl().Rekey(ctx, bootSecret, execdctl.Rekey{
		Generation: 1, AccessToken: "clone", ControlSecret: nextSecret,
		Envs: map[string]string{"SBX_REKEY_VAR": "clone-env"},
	})
	if err != nil {
		t.Fatalf("rekey: %v", err)
	}

	if st, out := s.runEcho("parent", "hi"); st != http.StatusUnauthorized {
		t.Fatalf("the parent's token still works after the re-key: %d %s", st, out)
	}

	if st, out := s.runEcho("clone", "$SBX_REKEY_VAR"); st != http.StatusOK || out != "clone-env" {
		t.Fatalf("new token: %d %q, want 200 and the re-key's env", st, out)
	}

	// The boot secret is dead: a different re-key under it is refused, whatever its generation.
	err = s.ctl().Rekey(ctx, bootSecret, execdctl.Rekey{Generation: 9, AccessToken: "x", ControlSecret: bootSecret + "zz"})
	if code(err) != codeUnauthorized {
		t.Fatalf("re-key under the retired secret: %v, want UNAUTHORIZED", err)
	}

	// The new secret works, but not to go backwards.
	err = s.ctl().Rekey(ctx, nextSecret, execdctl.Rekey{Generation: 1, AccessToken: "x", ControlSecret: bootSecret + "yy"})
	if code(err) != execdctl.CodeStaleGeneration {
		t.Fatalf("re-key with an old generation: %v, want STALE_GENERATION", err)
	}
}

// The retry for a lost response: the same request under the same secret succeeds again and
// changes nothing. A different request under that secret is not a retry and is refused.
func TestRekeyIsIdempotent(t *testing.T) {
	s := newTestServer(t, Options{AccessToken: "parent", ControlSecret: bootSecret})
	ctx := context.Background()
	rk := execdctl.Rekey{Generation: 4, AccessToken: "clone", ControlSecret: nextSecret}

	for i := range 3 {
		if err := s.ctl().Rekey(ctx, bootSecret, rk); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}

	if st, _ := s.runEcho("clone", "hi"); st != http.StatusOK {
		t.Fatalf("token after three identical re-keys: %d", st)
	}

	other := rk
	other.AccessToken = "someone-else"

	if err := s.ctl().Rekey(ctx, bootSecret, other); code(err) != codeUnauthorized {
		t.Fatalf("a different re-key under the retired secret: %v, want UNAUTHORIZED", err)
	}

	if st, _ := s.runEcho("someone-else", "hi"); st != http.StatusUnauthorized {
		t.Fatal("the refused re-key's token works")
	}
}

// Seal is what a snapshot captures: every clone wakes refusing clients, /ping included so the
// daemon's health probe reads "not serving", until its own re-key. A clone can therefore never
// answer anyone with its parent's token, even if the host forgot to re-key it.
func TestSealRefusesUntilRekey(t *testing.T) {
	s := newTestServer(t, Options{AccessToken: "parent", ControlSecret: bootSecret})
	ctx := context.Background()

	if err := s.ctl().Seal(ctx, bootSecret); err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Twice is harmless: the host may retry.
	if err := s.ctl().Seal(ctx, bootSecret); err != nil {
		t.Fatalf("second seal: %v", err)
	}

	st, _, body := s.do("POST", "/command", map[string]any{"command": "echo hi"}, AccessTokenHeader, "parent")
	wantError(t, st, body, http.StatusServiceUnavailable, execdctl.CodeSealed)

	if st, _, _ := s.do("GET", "/ping", nil); st != http.StatusServiceUnavailable {
		t.Fatalf("/ping while sealed: %d, want 503", st)
	}

	// The claim is a client call too.
	st, _, body = s.do("POST", "/sbx/claim", map[string]any{"accessToken": "x"}, AccessTokenHeader, "parent")
	wantError(t, st, body, http.StatusServiceUnavailable, execdctl.CodeSealed)

	// The old secret stays good for the re-key: it is what the host holds for this snapshot.
	if err := s.ctl().Rekey(ctx, bootSecret, execdctl.Rekey{Generation: 1, AccessToken: "clone", ControlSecret: nextSecret}); err != nil {
		t.Fatalf("rekey after seal: %v", err)
	}

	if st, _, _ := s.do("GET", "/ping", nil); st != http.StatusOK {
		t.Fatalf("/ping after the re-key: %d", st)
	}

	if st, _ := s.runEcho("clone", "hi"); st != http.StatusOK {
		t.Fatalf("the clone's token after the re-key: %d", st)
	}
}

// Seal forgets the idempotency record: a replay of the parent's last re-key - which the
// snapshot captured, so every clone has it - must not unseal a clone.
func TestReplayDoesNotUnseal(t *testing.T) {
	s := newTestServer(t, Options{AccessToken: "t0", ControlSecret: bootSecret})
	ctx := context.Background()
	first := execdctl.Rekey{Generation: 1, AccessToken: "t1", ControlSecret: nextSecret}

	if err := s.ctl().Rekey(ctx, bootSecret, first); err != nil {
		t.Fatal(err)
	}

	if err := s.ctl().Seal(ctx, nextSecret); err != nil {
		t.Fatal(err)
	}

	if err := s.ctl().Rekey(ctx, bootSecret, first); code(err) != codeUnauthorized {
		t.Fatalf("replay after a seal: %v, want UNAUTHORIZED", err)
	}

	if err := s.ctl().Rekey(ctx, nextSecret, first); code(err) != execdctl.CodeStaleGeneration {
		t.Fatalf("same generation under the current secret: %v, want STALE_GENERATION", err)
	}

	if st, _, _ := s.do("GET", "/ping", nil); st != http.StatusServiceUnavailable {
		t.Fatalf("/ping: %d, want still sealed", st)
	}
}

// A stream authorised by the old token does not outlive it: the re-key ends it, the same way a
// dropped client connection would.
func TestRekeyEndsOldTokensStreams(t *testing.T) {
	s := newTestServer(t, Options{AccessToken: "parent", ControlSecret: bootSecret})

	done := make(chan time.Duration, 1)

	go func() {
		start := time.Now()
		s.do("POST", "/command", map[string]any{"command": "sleep 20"}, AccessTokenHeader, "parent")
		done <- time.Since(start)
	}()

	time.Sleep(300 * time.Millisecond)

	if err := s.ctl().Rekey(context.Background(), bootSecret,
		execdctl.Rekey{Generation: 1, AccessToken: "clone", ControlSecret: nextSecret}); err != nil {
		t.Fatal(err)
	}

	select {
	case el := <-done:
		if el > 10*time.Second {
			t.Fatalf("the old token's stream ran %v", el)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stream authorised by the retired token is still running after the re-key")
	}
}

// Re-keying the parent with its own token - it keeps its identity after being snapshotted - is
// not a change of auth, and must not cut what it is serving.
func TestRekeyWithSameTokenKeepsStreams(t *testing.T) {
	s := newTestServer(t, Options{AccessToken: "parent", ControlSecret: bootSecret})

	done := make(chan struct{})

	go func() {
		defer close(done)
		s.do("POST", "/command", map[string]any{"command": "sleep 1"}, AccessTokenHeader, "parent")
	}()

	time.Sleep(200 * time.Millisecond)

	start := time.Now()

	if err := s.ctl().Rekey(context.Background(), bootSecret,
		execdctl.Rekey{Generation: 1, AccessToken: "parent", ControlSecret: nextSecret}); err != nil {
		t.Fatal(err)
	}

	<-done

	if el := time.Since(start); el < 500*time.Millisecond {
		t.Fatalf("the stream ended %v after a re-key that kept the token", el)
	}
}

// Requests racing a re-key see exactly one of the two tokens as valid at any instant; once
// Rekey has returned, the old one is refused and the new one accepted - never both, never
// neither. And re-keys racing each other under one secret: exactly one wins.
func TestRekeyUnderConcurrentRequests(t *testing.T) {
	s := newTestServer(t, Options{AccessToken: "parent", ControlSecret: bootSecret})
	ctx := context.Background()

	stop := make(chan struct{})

	var (
		wg       sync.WaitGroup
		rekeyed  atomic.Bool
		badAfter atomic.Int32
		other    atomic.Int32
	)

	for i := range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			tok := "parent"
			if i%2 == 1 {
				tok = "clone"
			}

			for {
				select {
				case <-stop:
					return
				default:
				}

				after := rekeyed.Load()
				st, _, _ := s.do("GET", "/directories/list?path=/", nil, AccessTokenHeader, tok)

				switch {
				case st != http.StatusOK && st != http.StatusUnauthorized:
					other.Add(1)
				case after && tok == "parent" && st == http.StatusOK:
					badAfter.Add(1)
				case after && tok == "clone" && st != http.StatusOK:
					badAfter.Add(1)
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)

	var (
		rwg  sync.WaitGroup
		wins atomic.Int32
	)

	for i := range 6 {
		rwg.Add(1)

		go func() {
			defer rwg.Done()

			// Only i==0 carries the token the readers expect; any winner is fine for the
			// exactly-one count, so all use "clone".
			err := s.ctl().Rekey(ctx, bootSecret, execdctl.Rekey{
				Generation: uint64(i + 1), AccessToken: "clone",
				ControlSecret: fmt.Sprintf("%s-%d", nextSecret, i),
			})
			if err == nil {
				wins.Add(1)
			}
		}()
	}

	rwg.Wait()
	rekeyed.Store(true)
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	if n := wins.Load(); n != 1 {
		t.Fatalf("%d concurrent re-keys under one secret succeeded, want exactly 1", n)
	}

	if n := badAfter.Load(); n != 0 {
		t.Fatalf("%d requests after the re-key saw the wrong token accepted or refused", n)
	}

	if n := other.Load(); n != 0 {
		t.Fatalf("%d requests got neither 200 nor 401 while re-keying", n)
	}
}

func TestRekeyRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("no control secret configured", func(t *testing.T) {
		s := newTestServer(t, Options{AccessToken: "t"})

		err := s.ctl().Rekey(ctx, "", execdctl.Rekey{Generation: 1, AccessToken: "x", ControlSecret: nextSecret})
		if code(err) != execdctl.CodeControlDisabled {
			t.Fatalf("got %v, want CONTROL_DISABLED", err)
		}

		if err := s.ctl().Seal(ctx, ""); code(err) != execdctl.CodeControlDisabled {
			t.Fatalf("seal: got %v, want CONTROL_DISABLED", err)
		}
	})

	cases := []struct {
		name   string
		secret string
		rk     execdctl.Rekey
		want   string
	}{
		{"no secret header", "", execdctl.Rekey{Generation: 1, AccessToken: "x", ControlSecret: nextSecret}, codeUnauthorized},
		{"wrong secret", "wrong", execdctl.Rekey{Generation: 1, AccessToken: "x", ControlSecret: nextSecret}, codeUnauthorized},
		{"generation 0", bootSecret, execdctl.Rekey{AccessToken: "x", ControlSecret: nextSecret}, execdctl.CodeStaleGeneration},
		{"no token", bootSecret, execdctl.Rekey{Generation: 1, ControlSecret: nextSecret}, codeInvalidRequest},
		{"short secret", bootSecret, execdctl.Rekey{Generation: 1, AccessToken: "x", ControlSecret: "short"}, codeInvalidRequest},
		{"secret not rotated", bootSecret, execdctl.Rekey{Generation: 1, AccessToken: "x", ControlSecret: bootSecret}, codeInvalidRequest},
		{"hidden env", bootSecret, execdctl.Rekey{Generation: 1, AccessToken: "x", ControlSecret: nextSecret,
			Envs: map[string]string{EnvAccessToken: "leak"}}, codeInvalidRequest},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t, Options{AccessToken: "t", ControlSecret: bootSecret})

			if err := s.ctl().Rekey(ctx, c.secret, c.rk); code(err) != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}

			// And nothing changed: the old token still works.
			if st, _ := s.runEcho("t", "hi"); st != http.StatusOK {
				t.Fatalf("a refused re-key changed the token: %d", st)
			}
		})
	}

	t.Run("seal with the wrong secret", func(t *testing.T) {
		s := newTestServer(t, Options{AccessToken: "t", ControlSecret: bootSecret})

		if err := s.ctl().Seal(ctx, "wrong"); code(err) != codeUnauthorized {
			t.Fatalf("got %v, want UNAUTHORIZED", err)
		}

		if st, _, _ := s.do("GET", "/ping", nil); st != http.StatusOK {
			t.Fatal("a refused seal sealed")
		}
	})
}

// When execd serves vsock, the control endpoints answer only there: the TCP listener is reachable
// by anything on the sandbox's network, the vsock one only by the VM's host.
func TestControlOnlyOverVsock(t *testing.T) {
	s := newTestServer(t, Options{AccessToken: "t", ControlSecret: bootSecret, ControlOverVsockOnly: true})

	err := s.ctl().Rekey(context.Background(), bootSecret, execdctl.Rekey{Generation: 1, AccessToken: "x", ControlSecret: nextSecret})
	if code(err) != execdctl.CodeControlTransport {
		t.Fatalf("re-key over TCP: %v, want CONTROL_TRANSPORT", err)
	}

	if err := s.ctl().Seal(context.Background(), bootSecret); code(err) != execdctl.CodeControlTransport {
		t.Fatalf("seal over TCP: %v, want CONTROL_TRANSPORT", err)
	}

	// The same server, with its connections marked as vsock the way connContext marks them.
	srv, err := New(Options{AccessToken: "t", ControlSecret: bootSecret, ControlOverVsockOnly: true, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewUnstartedServer(srv)
	ts.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, transportKey{}, true)
	}
	ts.Start()

	t.Cleanup(func() { ts.Close(); srv.Close() })

	v := &testServer{t: t, srv: srv, ts: ts}

	if err := v.ctl().Rekey(context.Background(), bootSecret,
		execdctl.Rekey{Generation: 1, AccessToken: "x", ControlSecret: nextSecret}); err != nil {
		t.Fatalf("re-key over (marked) vsock: %v", err)
	}
}

// Envs replace what identity calls set, and nothing else: a clone of a claimed sandbox gets the
// env its host sends, not its parent's caller's. nil leaves it alone.
func TestRekeyEnvReplacesIdentityEnv(t *testing.T) {
	for _, k := range []string{"SBX_ID_A", "SBX_ID_B", "SBX_IMAGE_VAR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	t.Setenv("SBX_IMAGE_VAR", "from-image")

	s := newTestServer(t, Options{AccessToken: "pool", ControlSecret: bootSecret})
	ctx := context.Background()

	st, _, body := s.do("POST", "/sbx/claim", map[string]any{"accessToken": "c1", "envs": map[string]string{"SBX_ID_A": "a"}},
		AccessTokenHeader, "pool")
	if st != http.StatusNoContent {
		t.Fatalf("claim: %d %s", st, body)
	}

	if err := s.ctl().Rekey(ctx, bootSecret, execdctl.Rekey{Generation: 1, AccessToken: "c2", ControlSecret: nextSecret,
		Envs: map[string]string{"SBX_ID_B": "b"}}); err != nil {
		t.Fatal(err)
	}

	if _, out := s.runEcho("c2", "${SBX_ID_A:-unset}-$SBX_ID_B-$SBX_IMAGE_VAR"); out != "unset-b-from-image" {
		t.Fatalf("env after re-key: %q, want the claim's key gone, the re-key's set, the image's kept", out)
	}

	if err := s.ctl().Rekey(ctx, nextSecret, execdctl.Rekey{Generation: 2, AccessToken: "c3",
		ControlSecret: bootSecret + "-3"}); err != nil {
		t.Fatal(err)
	}

	if _, out := s.runEcho("c3", "$SBX_ID_B"); out != "b" {
		t.Fatalf("env after a re-key with nil envs: %q, want it untouched", out)
	}
}

// A re-key changes who the sandbox is, not what it is for: a clone of an unclaimed pool member
// is still claimable (with its new token), a clone of a claimed one is not.
func TestRekeyKeepsClaimState(t *testing.T) {
	s := newTestServer(t, Options{AccessToken: "pool", ControlSecret: bootSecret})

	if err := s.ctl().Rekey(context.Background(), bootSecret,
		execdctl.Rekey{Generation: 1, AccessToken: "pool2", ControlSecret: nextSecret}); err != nil {
		t.Fatal(err)
	}

	if st, _, body := s.do("POST", "/sbx/claim", map[string]any{"accessToken": "mine"}, AccessTokenHeader, "pool2"); st != http.StatusNoContent {
		t.Fatalf("claim of a re-keyed pool member: %d %s", st, body)
	}
}

// The control secret is execd's own, like the token: a command never sees it.
func TestControlSecretHiddenFromCommands(t *testing.T) {
	t.Setenv(execdctl.EnvControlSecret, bootSecret)

	s := newTestServer(t, Options{AccessToken: "t", ControlSecret: bootSecret})

	if _, out := s.runEcho("t", "${"+execdctl.EnvControlSecret+":-hidden}"); out != "hidden" {
		t.Fatalf("a command saw the control secret: %q", out)
	}
}

// The host's whole path, short of a VM: execdctl over fcvsock's handshake, through a stand-in for
// Firecracker's unix socket that splices to execd. Seal and re-key must survive the handshake -
// in particular, nothing the handshake reads may eat the start of the HTTP response.
func TestRekeyThroughFirecrackerHandshake(t *testing.T) {
	s := newTestServer(t, Options{AccessToken: "parent", ControlSecret: bootSecret})
	target := strings.TrimPrefix(s.ts.URL, "http://")

	dir, err := os.MkdirTemp("", "fcx")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	uds := dir + "/v.sock"

	ln, err := net.Listen("unix", uds)
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

			go func() {
				defer c.Close()

				line := make([]byte, 0, 32)
				b := make([]byte, 1)

				for {
					if _, err := c.Read(b); err != nil {
						return
					}

					if b[0] == '\n' {
						break
					}

					line = append(line, b[0])
				}

				if string(line) != "CONNECT 44772" {
					return
				}

				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()

				_, _ = io.WriteString(c, "OK 1073741825\n")

				go func() { _, _ = io.Copy(up, c) }()

				_, _ = io.Copy(c, up)
			}()
		}
	}()

	ctl := execdctl.Client{Dial: fcvsock.Dialer{UDSPath: uds, Port: 44772}.DialContext}
	ctx := context.Background()

	if err := ctl.Seal(ctx, bootSecret); err != nil {
		t.Fatalf("seal through the handshake: %v", err)
	}

	if st, _, _ := s.do("GET", "/ping", nil); st != http.StatusServiceUnavailable {
		t.Fatalf("/ping after seal: %d", st)
	}

	if err := ctl.Rekey(ctx, bootSecret, execdctl.Rekey{Generation: 1, AccessToken: "clone", ControlSecret: nextSecret}); err != nil {
		t.Fatalf("re-key through the handshake: %v", err)
	}

	if st, _ := s.runEcho("clone", "hi"); st != http.StatusOK {
		t.Fatalf("clone token after the re-key: %d", st)
	}
}
