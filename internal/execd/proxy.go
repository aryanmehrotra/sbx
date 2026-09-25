//go:build unix

package execd

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"
)

// proxyPrefix is where any other port of the sandbox is reached: /proxy/{port}/rest goes to
// http://127.0.0.1:{port}/rest inside the sandbox. It is how a client gets at a service the
// sandbox runs without that port being published, and it is what sbx serve's endpoint for a
// port other than execd's resolves to.
const proxyPrefix = "/proxy/"

// proxyTransport is shared so keep-alive connections to a service survive between requests.
// It never uses the environment's proxy settings: the target is loopback by construction.
var proxyTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   100,
	IdleConnTimeout:       90 * time.Second,
	ExpectContinueTimeout: time.Second,
}

// proxy serves /proxy/{port}/... It is dispatched before the ServeMux, as upstream dispatches it
// before its router: the mux cleans paths and redirects "//" and "..", which would rewrite
// requests meant for the service rather than for execd.
//
// It sits behind the access token like every route but /ping. Upstream's proxy middleware runs
// after its token middleware, so the token is required there too; the token itself is not
// forwarded, because the service behind the proxy is the sandbox's workload and has no business
// holding the credential for the whole sandbox.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, proxyPrefix)
	portStr, _, _ := strings.Cut(rest, "/")

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("%q is not a port; the form is /proxy/{port}/path, e.g. /proxy/8080/", portStr))

		return
	}

	target := net.JoinHostPort("127.0.0.1", portStr)

	// The escaped form, so an encoded slash in the service's path reaches it still encoded.
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), proxyPrefix+portStr)
	if escaped == "" {
		escaped = "/"
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target
			pr.Out.URL.Path = "/" + strings.TrimPrefix(strings.TrimPrefix(rest, portStr), "/")
			pr.Out.URL.RawPath = escaped
			pr.Out.URL.RawQuery = r.URL.RawQuery

			// The incoming Host is kept, as upstream keeps it: dev servers that check Host
			// against what the browser used would otherwise refuse the request.
			pr.Out.Host = r.Host

			pr.SetXForwarded()
			pr.Out.Header.Del(AccessTokenHeader)
		},
		Transport: proxyTransport,
		// Flush at once: a proxied event stream or long poll must not sit in a buffer.
		FlushInterval: -1,
		ErrorLog:      s.log,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Printf("proxy %s %s -> %s: %v", r.Method, r.URL.Path, target, err)
			writeError(w, http.StatusBadGateway, codeRuntimeError,
				fmt.Sprintf("could not reach %s inside the sandbox: %v; check that the service is running and listening on port %d",
					target, err, port))
		},
	}

	rp.ServeHTTP(w, r)
}
