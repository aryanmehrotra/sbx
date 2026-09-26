package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Filter is the data-path component a real egress allow-list needs - the one SPEC.md and
// the spec's Egress field call for by name ("a filtering proxy in the data path, a component
// with a lifecycle, not a flag"). A filtered service sits on the same no-NAT bridge that
// `egress: "deny"` uses, so it has no route off the host on its own; this proxy, reachable
// on the bridge gateway, is the only way out, and it forwards only what its policy permits. A
// client that ignores the proxy and dials a host directly gets no route at all, so the policy is
// enforced rather than advisory - the point the spec comment makes about a control that controls.
//
// It answers CONNECT (the tunnel every HTTPS client opens) and plain HTTP. A destination the
// policy refuses gets 403 and no connection; the proxy never opens a socket to it.
//
// The policy is swapped in place (SetPolicy), so a running sandbox's egress can change without
// recreating anything. A request is judged against the policy in force when it arrives; a
// tunnel already open is not cut by a later change, the same as a firewall that matches on new
// connections only.
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

	// Resolve looks a hostname up, or is nil for the system resolver. The filter resolves names
	// itself, checks every address, and dials the address it checked - never the name again,
	// which would let a second answer (DNS rebinding) walk around the first check.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	// Refuse widens what counts as host-local (see hostLocal) for a filter that runs on the very
	// host its sandbox is kept off: a microVM's filter is a listener on that host's bridge, so
	// "CONNECT 10.231.5.1:22" through it would reach the host's sshd, and 10.231.7.2 another
	// sandbox's guest - both of which the guest cannot reach on its own. Like loopback, an
	// address it reports is refused unless an allow rule names it. Nil adds nothing.
	Refuse func(netip.Addr) bool

	pol atomic.Pointer[compiled]

	once      sync.Once
	transport *http.Transport
}

// New builds a filter from egress_allow entries, each a host or host:port (the port is
// ignored). A host permits itself and its subdomains, as the field always has.
func New(allow []string) *Filter { return NewPolicy(FromAllowList(allow)) }

// NewPolicy builds a filter enforcing p. p is expected to be normalized (ParsePolicy or
// Normalize); a policy that is not is enforced as written, which for a malformed target means a
// rule that matches nothing.
func NewPolicy(p Policy) *Filter {
	f := &Filter{}
	f.pol.Store(compile(p))

	return f
}

// SetPolicy replaces the policy in force, atomically: a request sees the old policy or the new
// one, never a mixture.
func (f *Filter) SetPolicy(p Policy) error {
	n, err := p.Normalize()
	if err != nil {
		return err
	}

	f.pol.Store(compile(n))

	return nil
}

// Policy returns the policy in force.
func (f *Filter) Policy() Policy { return f.pol.Load().p }

// Permits reports whether host (bare or host:port) is permitted by name alone - the domain rules
// and the default for a name, the address rules for an IP literal. It does not resolve; the
// resolved-address check is applied when a request is actually carried.
func (f *Filter) Permits(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	c := f.pol.Load()

	if a, err := netip.ParseAddr(host); err == nil {
		return c.allowsAddr(a) && !f.refused(c, a)
	}

	return c.allowsName(host)
}

// refused reports an address Refuse keeps out that no allow rule names.
func (f *Filter) refused(c *compiled, a netip.Addr) bool {
	a = a.Unmap()

	return f.Refuse != nil && f.Refuse(a) && !contains(c.allow, a)
}

// errDenied is a destination the policy refuses, as opposed to one that could not be reached.
type errDenied struct{ why string }

func (e *errDenied) Error() string { return "egress not allowed: " + e.why }

// admit decides one destination and returns the addresses it may be dialled at.
func (f *Filter) admit(ctx context.Context, host string) ([]netip.Addr, error) {
	c := f.pol.Load()

	if a, err := netip.ParseAddr(host); err == nil {
		if !c.allowsAddr(a) || f.refused(c, a) {
			return nil, &errDenied{why: host}
		}

		return []netip.Addr{a}, nil
	}

	if !c.allowsName(host) {
		return nil, &errDenied{why: host}
	}

	addrs, err := f.resolve(ctx, host)
	if err != nil {
		return nil, err
	}

	// Every address, not the first: a name whose answer set includes one denied address is a
	// name that can be steered to it, and which one a client dials is not ours to choose.
	for _, a := range addrs {
		if c.deniesResolved(a) || f.refused(c, a) {
			return nil, &errDenied{why: fmt.Sprintf("%s resolves to %s, which the policy denies", host, a)}
		}
	}

	return addrs, nil
}

func (f *Filter) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if f.Resolve != nil {
		return f.Resolve(ctx, host)
	}

	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// dial opens a connection to host:port, admitted and at a checked address.
func (f *Filter) dial(ctx context.Context, host, port string) (net.Conn, error) {
	addrs, err := f.admit(ctx, host)
	if err != nil {
		return nil, err
	}

	d := net.Dialer{Timeout: 10 * time.Second}

	var last error

	for _, a := range addrs {
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), port))
		if err == nil {
			return c, nil
		}

		last = err
	}

	if last == nil {
		last = fmt.Errorf("%s has no addresses", host)
	}

	return nil, last
}

func (f *Filter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		f.tunnel(w, r)
		return
	}

	f.forward(w, r)
}

// refuse answers a request the policy does not permit, or one whose upstream failed.
func refuse(w http.ResponseWriter, err error) {
	var d *errDenied
	if errors.As(err, &d) {
		http.Error(w, d.Error(), http.StatusForbidden)
		return
	}

	http.Error(w, err.Error(), http.StatusBadGateway)
}

// tunnel handles CONNECT: check the destination, and only then open the upstream socket and
// splice.
func (f *Filter) tunnel(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, "443"
	}

	upstream, err := f.dial(r.Context(), host, port)
	if err != nil {
		refuse(w, err)
		return
	}
	defer upstream.Close()

	f.note()

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

// forward handles a plain (non-CONNECT) HTTP request: check the destination, then relay it.
func (f *Filter) forward(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()
	if host == "" {
		host = r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}

	// Checked per request, not only at dial: the transport pools connections, and one opened
	// under an older, looser policy must not carry a request the current policy refuses.
	if _, err := f.admit(r.Context(), host); err != nil {
		refuse(w, err)
		return
	}

	f.note()

	r.RequestURI = ""

	resp, err := f.roundTripper().RoundTrip(r)
	if err != nil {
		refuse(w, err)
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

// roundTripper is the transport plain HTTP is relayed on. Its dialer re-admits and dials a
// checked address, so the name is never resolved by anything but the filter.
func (f *Filter) roundTripper() *http.Transport {
	f.once.Do(func() {
		f.transport = &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}

				return f.dial(ctx, host, port)
			},
			MaxIdleConns:        16,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}
	})

	return f.transport
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
