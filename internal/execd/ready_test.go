//go:build unix

package execd

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/jupyter"
)

func getPing(t *testing.T, s *testServer, query string) (int, string) {
	t.Helper()

	resp, err := http.Get(s.ts.URL + "/ping" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(b)
}

// A sandbox whose image configures Jupyter is Running only once Jupyter answers: a microVM cold
// boot plus Jupyter's own start is slower than docker, and the API reported Running while the
// first /code call then spent its caller's whole deadline waiting inside execd. /ping?ready=code
// is the readiness the API asks for; plain /ping stays the liveness probe it always was.
func TestPingReadyCodeWaitsForAConfiguredJupyter(t *testing.T) {
	f := newMiniJupyter(t)
	f.downUntil.Store(time.Now().Add(time.Hour).UnixNano()) // still starting
	s := newTestServer(t, Options{Jupyter: f.engine()})

	if code, _ := getPing(t, s, ""); code != http.StatusOK {
		t.Fatalf("liveness /ping = %d while Jupyter starts, want 200", code)
	}

	code, body := getPing(t, s, "?ready=code")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "Jupyter") || !strings.Contains(body, codeJupyterNotReady) {
		t.Fatalf("/ping?ready=code with Jupyter not up = %d %q, want 503 naming Jupyter", code, body)
	}

	f.downUntil.Store(0)

	if code, body := getPing(t, s, "?ready=code"); code != http.StatusOK {
		t.Fatalf("/ping?ready=code with Jupyter up = %d %q", code, body)
	}
}

// An image with no Jupyter is ready when execd is: nothing about a non-Jupyter image slows down.
func TestPingReadyCodeIsImmediateWithoutJupyter(t *testing.T) {
	s := newTestServer(t, Options{Jupyter: jupyter.New(jupyter.Config{})})

	start := time.Now()
	if code, _ := getPing(t, s, "?ready=code"); code != http.StatusOK || time.Since(start) > time.Second {
		t.Fatalf("/ping?ready=code without Jupyter = %d after %s", code, time.Since(start))
	}
}
