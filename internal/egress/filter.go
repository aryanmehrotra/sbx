package egress

import (
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Filter is the data-path component a real egress allow-list needs - the one SPEC.md and
// the spec's Egress field call for by name ("a filtering proxy in the data path, a component
// with a lifecycle, not a flag"). A service with an allow-list sits on the same no-NAT bridge
// that `egress: "deny"` uses, so it has no route off the host on its own; this proxy, reachable
// on the bridge gateway, is the only way out, and it forwards only to the allowed hosts. A
// client that ignores the proxy and dials a host directly gets no route at all, so the list is
// enforced rather than advisory - the point the spec comment makes about a control that controls.
//
// It answers CONNECT (the tunnel every HTTPS client opens) and plain HTTP. A host that is not on
// the list gets 403 and no connection; the proxy never opens a socket to it.
type Filter struct {
	// OnActivity is called when a permitted request is carried, or nil.
	//
	// This is the idle signal for a box that nothing dials. sbx measures idleness on bytes
	// through its proxy, and an agent working inside a sandbox sends none of them - it reads
	// files, compiles, and calls an API. The only one of those sbx can see is the API call,
	// and for a box with an allow-list that call comes through here.
	//
	// Without it the only setting that kept such a box alive was idle: "never", which holds its
	// memory for the sandbox's whole life. With it, a box that is working stays awake and one
	// that has stopped sleeps on the ordinary timer.
	OnActivity func()

	// allow holds lower-cased host suffixes. "openai.com" permits openai.com and any subdomain
	// of it (api.openai.com), which is what an allow-list of a service's domain has to mean -
	// an API's endpoints move across subdomains and a per-host list would break on the first one.
	allow []string
}

// New builds a filter from allow entries, each a host or host:port (the port is
// ignored - the list matches on host). Blank and malformed entries are dropped.
func New(allow []string) *Filter {
	f := &Filter{}

	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if h, _, err := net.SplitHostPort(a); err == nil {
			a = h
		}

		if a = strings.TrimSuffix(a, "."); a != "" {
			f.allow = append(f.allow, a)
		}
	}

	return f
}

// Permits reports whether host (bare or host:port) is on the allow-list, matching the host
// itself and any subdomain of a listed one.
func (f *Filter) Permits(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	for _, a := range f.allow {
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}

	return false
}

func (f *Filter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		f.tunnel(w, r)
		return
	}

	f.forward(w, r)
}

// tunnel handles CONNECT: check the host, and only then open the upstream socket and splice.
func (f *Filter) tunnel(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, "443"
	}

	if !f.Permits(host) {
		http.Error(w, "egress not allowed: "+host, http.StatusForbidden)
		return
	}

	f.note()

	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 10*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT unsupported by this server", http.StatusInternalServerError)
		return
	}

	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Splice both directions; the first to finish closes both via the defers, which unblocks
	// the other copy. No half-open leak, and nothing on the wake path.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(f.active(upstream), client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(f.active(client), upstream); done <- struct{}{} }()
	<-done
}

// forward handles a plain (non-CONNECT) HTTP request: check the host, then relay it.
func (f *Filter) forward(w http.ResponseWriter, r *http.Request) {
	if !f.Permits(r.Host) {
		http.Error(w, "egress not allowed: "+r.Host, http.StatusForbidden)
		return
	}

	f.note()

	r.RequestURI = ""

	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(f.active(w), resp.Body)
}

// stamp reports activity as bytes move, not just when a request opens.
//
// A CONNECT to a model API can stay open for minutes while tokens stream back, and stamping
// only at admission would let the idle timer fire in the middle of one. sbx measures its own
// idleness on bytes rather than connections for exactly that reason; this is the same rule
// applied to the way out.
type stamp struct {
	w  io.Writer
	on func()
}

func (s stamp) Write(p []byte) (int, error) {
	s.on()
	return s.w.Write(p)
}

// active wraps w to report activity, or returns it untouched when nothing is listening - which
// is every filter built by a test, and every one built for a gateway whose units this process
// does not own.
func (f *Filter) active(w io.Writer) io.Writer {
	if f.OnActivity == nil {
		return w
	}

	return stamp{w: w, on: f.OnActivity}
}

// note stamps once for a request that carried no bytes at all: a 204, an empty body, a CONNECT
// that was opened and dropped. It still says the box is doing something.
func (f *Filter) note() {
	if f.OnActivity != nil {
		f.OnActivity()
	}
}
