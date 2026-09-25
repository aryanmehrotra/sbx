//go:build unix && !linux

package execd

import (
	"net/http"
	"testing"
)

// Off Linux every pty route answers 501 - the endpoint exists, this platform cannot serve it.
func TestPTYIs501OffLinux(t *testing.T) {
	s := newTestServer(t, Options{})

	for _, c := range []struct{ method, path string }{
		{"POST", "/pty"}, {"GET", "/pty/abc"}, {"DELETE", "/pty/abc"}, {"GET", "/pty/abc/ws"},
	} {
		status, _, body := s.do(c.method, c.path, nil)
		wantError(t, status, body, http.StatusNotImplemented, codeNotSupported)
	}
}
