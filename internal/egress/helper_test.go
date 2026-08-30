package egress

import (
	"net/http"
	"net/url"
	"testing"
)

// fixedProxy sends every request through u, so a test can drive the filter the way a container
// with HTTP_PROXY set does.
func fixedProxy(t *testing.T, u string) func(*http.Request) (*url.URL, error) {
	t.Helper()

	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}

	return func(*http.Request) (*url.URL, error) { return parsed, nil }
}
