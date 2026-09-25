package execdctl

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func clientFor(ts *httptest.Server) Client {
	return Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", strings.TrimPrefix(ts.URL, "http://"))
	}}
}

// What goes on the wire: the path, the secret header, and a body whose field names execd reads.
func TestRekeyWireShape(t *testing.T) {
	var got struct {
		path, secret, body string
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.path, got.secret, got.body = r.URL.Path, r.Header.Get(SecretHeader), string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	err := clientFor(ts).Rekey(context.Background(), "s0", Rekey{Generation: 3, AccessToken: "t", ControlSecret: "s1"})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"generation":3,"accessToken":"t","controlSecret":"s1","envs":null}`
	if got.path != PathRekey || got.secret != "s0" || got.body != want {
		t.Fatalf("sent %+v, want %s with secret s0 and body %s", got, PathRekey, want)
	}
}

// A refusal comes back typed, with execd's code; a body that is not execd's error shape is kept
// as the message so it can still be read.
func TestErrorsCarryTheCode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathSeal {
			http.Error(w, "proxy said no", http.StatusBadGateway)
			return
		}

		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"STALE_GENERATION","message":"send 5"}`)
	}))
	defer ts.Close()

	var e *Error

	err := clientFor(ts).Rekey(context.Background(), "s", Rekey{})
	if !errors.As(err, &e) || e.Status != http.StatusConflict || e.Code != CodeStaleGeneration || e.Message != "send 5" {
		t.Fatalf("got %#v", err)
	}

	err = clientFor(ts).Seal(context.Background(), "s")
	if !errors.As(err, &e) || e.Status != http.StatusBadGateway || e.Message != "proxy said no" {
		t.Fatalf("got %#v", err)
	}
}

func TestNoDial(t *testing.T) {
	if err := (Client{}).Seal(context.Background(), "s"); err == nil || !strings.Contains(err.Error(), "fcvsock") {
		t.Fatalf("got %v, want a refusal naming what to pass", err)
	}
}

func TestNewSecret(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}

	b, _ := NewSecret()

	if len(a) < MinSecretLen || a == b {
		t.Fatalf("NewSecret gave %q then %q", a, b)
	}
}
