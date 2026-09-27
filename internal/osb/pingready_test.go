package osb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Running means execd answers, and nothing about Jupyter. Upstream's server reports Running once
// the container runs (docker_service.py, release-1.1.0) and leaves Jupyter to its SDK, whose
// CreateCodeInterpreter polls the kernel port itself. Waiting for Jupyter here held a
// code-interpreter image whose entrypoint never starts one - upstream's e2e_test.go runs it with
// `tail -f /dev/null` - Pending until it failed.
func TestPingIsExecdLivenessWhateverJupyterSays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping" {
			http.Error(w, "wrong path "+r.URL.String(), http.StatusTeapot)
			return
		}

		// What execd answers to ?ready=code while an image-configured Jupyter is down.
		if r.URL.Query().Get("ready") == "code" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"JUPYTER_NOT_READY","message":"execd is up, but Jupyter is not answering yet: refused"}`))

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := pingExecd(context.Background(), strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatalf("ping with execd up and Jupyter down = %v, want Running", err)
	}
}

// A ping that fails carries execd's own sentence, not only the status line.
func TestPingCarriesExecdsReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"UNAVAILABLE","message":"execd is shutting down"}`))
	}))
	defer srv.Close()

	err := pingExecd(context.Background(), strings.TrimPrefix(srv.URL, "http://"))
	if err == nil || !strings.Contains(err.Error(), "execd is shutting down") {
		t.Fatalf("ping = %v, want execd's reason", err)
	}
}
