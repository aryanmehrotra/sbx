package osb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
