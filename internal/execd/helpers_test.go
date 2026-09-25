//go:build unix

package execd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type testServer struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
}

func newTestServer(t *testing.T, o Options) *testServer {
	t.Helper()

	if o.OutputDir == "" {
		o.OutputDir = t.TempDir()
	}

	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s)

	t.Cleanup(func() {
		ts.Close()
		s.Close()
	})

	return &testServer{t: t, srv: s, ts: ts}
}

// do sends a request and returns status, headers and body. body may be nil, a string, []byte
// or anything JSON-encodable.
func (s *testServer) do(method, path string, body any, headers ...string) (int, http.Header, []byte) {
	s.t.Helper()

	var rd io.Reader

	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			s.t.Fatal(err)
		}

		rd = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, s.ts.URL+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}

	return resp.StatusCode, resp.Header, data
}

// run POSTs a command and parses the stream the way the SDK does.
func (s *testServer) run(body any) *sdkExecution {
	s.t.Helper()

	return s.stream("POST", "/command", body)
}

func (s *testServer) stream(method, path string, body any) *sdkExecution {
	s.t.Helper()

	status, h, data := s.do(method, path, body)
	if status != http.StatusOK {
		s.t.Fatalf("%s %s: status %d, body %s", method, path, status, data)
	}

	if ct := h.Get("Content-Type"); ct != "text/event-stream" {
		s.t.Fatalf("%s %s: Content-Type %q, want text/event-stream", method, path, ct)
	}

	exec, err := parseExecution(bytes.NewReader(data))
	if err != nil {
		s.t.Fatalf("%s %s: SDK parser rejected the stream: %v\n%s", method, path, err, data)
	}

	return exec
}

// wantError asserts a JSON error response with the given status and code.
func wantError(t *testing.T, status int, body []byte, wantStatus int, wantCode string) {
	t.Helper()

	if status != wantStatus {
		t.Fatalf("status %d, want %d; body %s", status, wantStatus, body)
	}

	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body is not the spec's ErrorResponse: %v; body %q", err, body)
	}

	if e.Code != wantCode || e.Message == "" {
		t.Fatalf("error = %+v, want code %s and a message", e, wantCode)
	}
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("%s did not happen within %s", what, within)
}
