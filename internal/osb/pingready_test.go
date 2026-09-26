package osb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Running means usable. For an image that configures Jupyter that is execd AND Jupyter, so the
// API asks execd for readiness (?ready=code), and a sandbox whose Jupyter never comes up fails
// with execd's own sentence about it rather than "execd /ping answered 503".
func TestPingAsksForReadinessAndCarriesExecdsReason(t *testing.T) {
	up := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping" || r.URL.Query().Get("ready") != "code" {
			http.Error(w, "wrong probe "+r.URL.String(), http.StatusTeapot)
			return
		}

		if !up {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"NOT_SUPPORTED","message":"execd is up, but the image's Jupyter server (JUPYTER_HOST) is not answering yet: refused"}`))

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hostport := strings.TrimPrefix(srv.URL, "http://")

	err := pingExecd(context.Background(), hostport)
	if err == nil || !strings.Contains(err.Error(), "Jupyter server (JUPYTER_HOST) is not answering yet") {
		t.Fatalf("ping while Jupyter starts = %v, want execd's reason", err)
	}

	up = true
	if err := pingExecd(context.Background(), hostport); err != nil {
		t.Fatalf("ping once ready = %v", err)
	}
}

// execd answering while Jupyter does not is its own error, so the API can tell "the sandbox is
// up, its Jupyter is not" from "nothing answers".
func TestPingSaysWhenOnlyJupyterIsNotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"JUPYTER_NOT_READY","message":"execd is up, but Jupyter is not answering yet: refused"}`))
	}))
	defer srv.Close()

	err := pingExecd(context.Background(), strings.TrimPrefix(srv.URL, "http://"))

	var nr *jupyterNotReady
	if !errors.As(err, &nr) {
		t.Fatalf("ping = %v (%T), want a jupyterNotReady", err, err)
	}
}

// When execd is up and Jupyter is not, the create waits for Jupyter past the execd deadline -
// bounded by its own timeout - and a Jupyter that never answers fails with a cause that says
// exactly that and when execd came up.
func TestAJupyterThatNeverAnswersFailsSayingSo(t *testing.T) {
	h := newHarness(t, func(_ *harness, o *Options) {
		o.ReadyTimeout = 200 * time.Millisecond
		o.CodeReadyTimeout = 5 * time.Second
	})

	h.mu.Lock()
	h.pingErr = &jupyterNotReady{msg: "execd is up, but Jupyter is not answering yet: connection refused"}
	h.mu.Unlock()

	var created sandboxJSON
	h.do("POST", "/v1/sandboxes", minimalCreate(), &created)

	// Past the execd deadline but inside Jupyter's: still Pending, not failed.
	time.Sleep(100 * time.Millisecond)
	h.advance(time.Second)
	time.Sleep(100 * time.Millisecond)

	var got sandboxJSON
	if h.do("GET", "/v1/sandboxes/"+created.ID, nil, &got); got.Status.State != statePending {
		t.Fatalf("with execd up and Jupyter starting, past the execd deadline: %+v", got.Status)
	}

	h.advance(10 * time.Second)

	got = h.waitState(created.ID, stateFailed)
	if got.Status.Reason != "provision_timeout" ||
		!strings.Contains(got.Status.Message, "Jupyter did not answer within 5s (execd up after") ||
		!strings.Contains(got.Status.Message, "connection refused") {
		t.Fatalf("status %+v", got.Status)
	}
}
