package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
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

	// Refuse is what this filter will never dial, whatever the policy says. Every filter sbx runs
	// sits somewhere its sandbox is kept off: a microVM's filter is a listener on the host's bridge,
	// so "CONNECT 10.231.5.1:22" through it would reach the host's sshd, 127.0.0.1 the host's own
	// loopback services, and 10.231.7.2 another sandbox's guest. A container filter on colima or
	// Docker Desktop is one hop from the VM and, through host.lima.internal, from the Mac's
	// loopback (Doors). None of that is reachable from the workload on its own.
	//
	// No allow rule opens it. The policy is the sandbox's - its caller writes it through the API -
	// and a sandbox must not be able to write itself a door onto the machine that runs its filter;
	// widening this set is the operator's (sbx serve), never the policy's. Nil refuses nothing
	// beyond the policy's own host-local default (hostLocal, which an allow rule does open): that
	// is right only for a filter whose loopback is its own and that has nothing else behind it,
	// which today is only a test.
	Refuse func(netip.Addr) bool

	pol atomic.Pointer[compiled]

	// ports are the ports this filter carries beyond DefaultPorts (80 and 443), each for one
	// target, from egress_allow entries written as host:port (SetPorts). Nothing else crosses: a
	// proxy that tunnels a CONNECT to any port is a raw TCP relay, and under a default of allow
	// that is a way to SSH, SMTP or a database anywhere, which SPEC.md has always said it is not.
	ports atomic.Pointer[[]PortGrant]

	once      sync.Once
	transport *http.Transport
}

// New builds a filter from egress_allow entries, each a host or host:port. A host permits itself
// and its subdomains, as the field always has, on ports 80 and 443; a port on an entry adds that
// port for that host.
func New(allow []string) *Filter {
	f := NewPolicy(FromAllowList(allow))
	f.SetPorts(PortGrantsFromAllowList(allow))

	return f
}

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

// SetPorts replaces the ports carried beyond 80 and 443, atomically like SetPolicy.
func (f *Filter) SetPorts(g []PortGrant) { f.ports.Store(&g) }

// Ports returns the ports carried beyond 80 and 443.
func (f *Filter) Ports() []PortGrant {
	if g := f.ports.Load(); g != nil {
		return *g
	}

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
		return c.allowsAddr(a) && !f.refused(a)
	}

	return c.allowsName(host)
}

// refused reports an address Refuse keeps out. The policy is not consulted: see Refuse.
func (f *Filter) refused(a netip.Addr) bool {
	return f.Refuse != nil && f.Refuse(a.Unmap())
}

// errDenied is a destination the policy refuses, as opposed to one that could not be reached.
type errDenied struct {
	why string

	// door is a refusal Refuse made: no policy or grant opens it, so nothing may suggest one.
	door bool
}

func (e *errDenied) Error() string { return "egress not allowed: " + e.why }

// carries refuses a port this filter does not carry for host. It is asked before any lookup, so a
// refused port costs no DNS query and opens no socket; check decides which refusal is given.
//
// host is what the client named. A grant for a name is matched against the name, never against
// an address it resolved to, so "github.com:22" does not open port 22 on whatever else shares
// github.com's addresses.
func (f *Filter) carries(host, port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return &errDenied{why: fmt.Sprintf("%q is not a port", port)}
	}

	if slices.Contains(DefaultPorts, uint16(n)) {
		return nil
	}

	for _, g := range f.Ports() {
		if int(g.Port) == n && g.covers(host) {
			return nil
		}
	}

	return &errDenied{why: fmt.Sprintf("port %d of %s: the egress filter carries ports 80 and 443 "+
		"only. To reach this port, list %q in the service's egress_allow and recreate the sandbox "+
		"(egress_policy and sbx egress have no port field)", n, host,
		PortGrant{Target: strings.ToLower(host), Port: uint16(n)}.String())}
}

// closedOff says why an address Refuse keeps out is refused, so a person reading the 403 does not
// go looking for the rule that denies it: there is none, and no rule would open it.
const closedOff = " - the machine the egress filter runs on, or one behind it, which no policy opens"

// admit decides one destination and returns the addresses it may be dialled at.
func (f *Filter) admit(ctx context.Context, host string) ([]netip.Addr, error) {
	c := f.pol.Load()

	if a, err := netip.ParseAddr(host); err == nil {
		if f.refused(a) {
			return nil, &errDenied{why: host + closedOff, door: true}
		}

		if !c.allowsAddr(a) {
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
		if f.refused(a) {
			return nil, &errDenied{why: fmt.Sprintf("%s resolves to %s%s", host, a, closedOff), door: true}
		}

		if c.deniesResolved(a) {
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

// check decides one request: the port, the destination, and which refusal to give when both
// fail. It returns the addresses host:port may be dialled at.
//
// The refusal has to be advice that works. The port used to be checked first, so a request to a
// door on an uncarried port - GET http://host.lima.internal:28777/ - was told to list
// host.lima.internal:28777 in egress_allow, which would then have been refused as a door. So an
// uncarried port still asks about the destination: a door says it is one, a host the policy
// denies says that, and only a host that would otherwise be let through gets the port hint.
//
// It asks only what it can answer without a lookup, because a refused port must cost no DNS
// query (TestDefaultAllowRefusesPortsOtherThanHTTPAndHTTPS): an address literal is judged in
// full, a name by the policy's name rules and by whether it is one of HostDoorNames. A name that
// resolves to a door only through some other record still gets the port hint; with the port
// granted, its next request is refused as a door, by address, and says so.
func (f *Filter) check(ctx context.Context, host, port string) ([]netip.Addr, error) {
	perr := f.carries(host, port)
	if perr == nil {
		return f.admit(ctx, host)
	}

	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, perr // not a port at all: nothing about the host would change the answer
	}

	var err error

	switch _, lerr := netip.ParseAddr(host); {
	case lerr == nil:
		_, err = f.admit(ctx, host) // a literal: no lookup
	case isDoorName(host):
		err = &errDenied{why: host + closedOff, door: true}
	case !f.pol.Load().allowsName(host):
		err = &errDenied{why: host}
	}

	var d *errDenied
	if errors.As(err, &d) {
		if d.door {
			return nil, d
		}

		return nil, &errDenied{why: d.why + fmt.Sprintf(" (port %s is not carried either)", port)}
	}

	return nil, perr
}

// isDoorName reports one of HostDoorNames, which name the machine behind a container filter.
func isDoorName(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")

	return slices.Contains(HostDoorNames, host)
}

// dial opens a connection to host:port, admitted and at a checked address.
func (f *Filter) dial(ctx context.Context, host, port string) (net.Conn, error) {
	addrs, err := f.check(ctx, host, port)
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

// setupTimeout bounds resolving and dialling a destination once that no longer follows the
// client's connection (see detach). The dialer's own 10s covers one address; this covers the
// lookup and every address together.
const setupTimeout = 30 * time.Second

// detach is the context upstream work runs under: the request's values, not its cancellation.
//
// net/http cancels a request's context as soon as its background read sees EOF from the client.
// A client that shuts its write side after sending is finished, not gone - busybox wget, the
// wget in every alpine image, does exactly that - and on r.Context() every lookup it caused was
// cancelled before it answered, so the filter refused an allowed destination with 502. A client
// that really has gone is still found out: the write back to it fails.
func detach(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

// tunnel handles CONNECT: check the destination, and only then open the upstream socket and
// splice.
func (f *Filter) tunnel(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, "443"
	}

	ctx, cancel := context.WithTimeout(detach(r), setupTimeout)
	upstream, err := f.dial(ctx, host, port)
	cancel()
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

	// A proxied request names its port in the URL; with none it is the scheme's.
	port := r.URL.Port()
	if port == "" {
		port = "80"
		if r.URL.Scheme == "https" {
			port = "443"
		}
	}

	// Checked per request, not only at dial: the transport pools connections, and one opened
	// under an older, looser policy must not carry a request the current policy refuses.
	admitCtx, cancel := context.WithTimeout(detach(r), setupTimeout)
	_, err := f.check(admitCtx, host, port)
	cancel()

	if err != nil {
		refuse(w, err)
		return
	}

	f.note()

	// Detached but not bounded: a model API can take minutes before its first header byte, and
	// the transport bounds its own dial and TLS handshake.
	r = r.WithContext(detach(r))
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
