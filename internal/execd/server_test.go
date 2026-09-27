//go:build unix

package execd

import (
	"net/http"
	"strings"
	"testing"
)

func TestAccessToken(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		path       string
		sent       []string
		want       int
	}{
		{"no token configured lets everything in", "", "/metrics", nil, http.StatusOK},
		{"missing header", "s3cret", "/metrics", nil, http.StatusUnauthorized},
		{"wrong token", "s3cret", "/metrics", []string{AccessTokenHeader, "s3cre"}, http.StatusUnauthorized},
		{"longer wrong token", "s3cret", "/metrics", []string{AccessTokenHeader, "s3cretX"}, http.StatusUnauthorized},
		{"right token", "s3cret", "/metrics", []string{AccessTokenHeader, "s3cret"}, http.StatusOK},
		{"ping needs no token", "s3cret", "/ping", nil, http.StatusOK},
		{"unknown path is still behind the token", "s3cret", "/nope", nil, http.StatusUnauthorized},
		{"501 stubs are behind the token", "s3cret", "/code/contexts", nil, http.StatusUnauthorized},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t, Options{AccessToken: c.configured})

			status, _, body := s.do("GET", c.path, nil, c.sent...)
			if c.want == http.StatusUnauthorized {
				wantError(t, status, body, http.StatusUnauthorized, codeUnauthorized)
				return
			}

			if status != c.want {
				t.Fatalf("status %d, want %d; body %s", status, c.want, body)
			}
		})
	}
}

func TestUnbuiltEndpointsAnswer501WithoutPromisingARelease(t *testing.T) {
	s := newTestServer(t, Options{})

	cases := []struct{ method, path, feature string }{
		{"POST", "/v1/isolated/session", "isolated sessions"},
		{"GET", "/v1/isolated/capabilities", "isolated sessions"},
	}

	for _, c := range cases {
		status, _, body := s.do(c.method, c.path, nil)
		wantError(t, status, body, http.StatusNotImplemented, codeNotSupported)

		if !strings.Contains(string(body), c.feature) {
			t.Errorf("%s %s: message does not name the missing feature (%s): %s", c.method, c.path, c.feature, body)
		}
		// v0.11.0 shipped without isolated sessions while this message said it would add them.
		// A release is named here only once it has shipped the endpoint, never in advance.
		if strings.Contains(string(body), "sbx v0.") {
			t.Errorf("%s %s: message promises a release: %s", c.method, c.path, body)
		}
	}
}

func TestUnknownPathIsJSON404(t *testing.T) {
	s := newTestServer(t, Options{})

	status, _, body := s.do("GET", "/definitely/not/here", nil)
	wantError(t, status, body, http.StatusNotFound, codeNotFound)

	status, _, body = s.do("GET", "/command/abc/other", nil)
	wantError(t, status, body, http.StatusNotFound, codeNotFound)
}

func TestPanicBecomes500(t *testing.T) {
	s := newTestServer(t, Options{})
	s.srv.mux.Handle("GET /boom", s.srv.guard(func(http.ResponseWriter, *http.Request) { panic("boom") }))

	status, _, body := s.do("GET", "/boom", nil)
	wantError(t, status, body, http.StatusInternalServerError, codeRuntimeError)
}
