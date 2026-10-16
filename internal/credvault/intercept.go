package credvault

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// handshakeTimeout bounds the TLS handshake with the sandbox's own client.
//
// A client that opened the tunnel and then sent nothing would otherwise hold the goroutine and the
// upstream-less connection for as long as it liked.
const handshakeTimeout = 30 * time.Second

// Intercept is the handler the daemon serves a hosted filter through: the filter itself for every
// request, except a CONNECT to a destination the vault has a binding for.
//
// The filter is taken as an http.Handler rather than an *egress.Filter because that is all this
// needs of it. Narrower on purpose: it keeps the import out (nothing here reaches into the data
// path's package), and it lets a test drive the termination without a resolver and a privileged
// port, which a filter would insist on - a binding only ever covers port 80 or 443.
//
// Why this is not in internal/egress, like everything else on the data path: every file of that
// package is compiled verbatim into the container filter (BuildContext), so a TLS stack there
// would be a TLS stack in a container on the sandbox's own bridge, holding a certificate
// authority's private key. The filter keeps no crypto and a test enforces it. So the termination
// lives here, beside the key, and the filter stays what it was.
//
// What this does NOT do is decide anything about egress. A terminated request is handed straight
// back to the filter, which resolves it, checks it against the current policy, adds the
// credentials and relays it - so a binding can only ever change what a permitted request carries,
// never where the sandbox may reach. There is deliberately no second copy of the policy check
// here: one would be a second answer to drift from the first.
func Intercept(filter http.Handler, vault *Vault) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			filter.ServeHTTP(w, r)
			return
		}

		host, port := targetOf(r.Host)

		// Bound, and only bound: an unbound destination keeps the splice it has always had.
		// Terminating every tunnel would make the filter a man in the middle of traffic it has
		// no business reading, and would need the sandbox to trust this authority for hosts no
		// binding names - which the authority's own name constraints forbid anyway.
		if vault == nil || !vault.Bound(host, port) {
			filter.ServeHTTP(w, r)
			return
		}

		leaf, err := vault.leafFor(host)
		if err != nil {
			// 502 and not 403: the policy is not what refused this. The destination is one the
			// sandbox asked to have credentials added to, and the authority that would let the
			// filter read it could not produce a certificate for it.
			http.Error(w, "the credential vault could not mint a certificate for "+host+
				", so this request was not sent: "+err.Error(), http.StatusBadGateway)

			return
		}

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

		if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}

		serveTerminated(client, leaf, filter, host, port)
	})
}

// serveTerminated runs one terminated tunnel: handshake as the destination, then serve the
// requests inside it through the filter.
//
// Both protocols the agent CLIs use are offered. Claude Code and Codex stream over HTTP/2, so a
// termination that only spoke HTTP/1.1 was a credential path the clients it exists for had never
// been through: whichever of them negotiated h2 would have found no ALPN agreement and fallen back
// at best, and the swap is the product's headline protection.
func serveTerminated(client net.Conn, leaf *tls.Certificate, filter http.Handler, host, port string) {
	// The listener wrapper goes UNDER the TLS layer rather than around it, which is what makes h2
	// reachable at all. net/http decides a connection's protocol by asking it for its
	// ConnectionState, so the thing http.Serve is handed has to BE the *tls.Conn - wrapped in
	// anything, the methods are not promoted, the ALPN result is never read, and every tunnel is
	// HTTP/1.1 whatever the handshake agreed. Closing still reaches the listener, because closing
	// a tls.Conn closes what it was built on.
	ln := &oneConn{}

	inside := tls.Server(closesListener{Conn: client, listener: ln}, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	})
	defer inside.Close()

	ln.conn = inside

	_ = inside.SetDeadline(time.Now().Add(handshakeTimeout))

	if err := inside.Handshake(); err != nil {
		// Nothing to answer on: a failed handshake has no HTTP to carry a message. The usual
		// cause is a sandbox that does not trust this authority, which `sbx doctor` and the
		// vault's own docs cover.
		return
	}

	// Cleared, because the requests inside a tunnel have no deadline of their own - a model API
	// can take minutes before its first header byte, which is the same reason forward detaches.
	_ = inside.SetDeadline(time.Time{})

	// http.Serve gives the decrypted stream a real HTTP server: keep-alive, pipelining, chunked
	// bodies, the HTTP/2 framing layer and correct response framing, none of which is worth
	// reimplementing here. It configures HTTP/2 for a listener it was given rather than one it
	// opened - a zero Server has no TLSConfig, which net/http reads as "the caller may hand me an
	// h2 conn" - so the only thing this has to get right is the NextProtos above and the
	// unwrapped conn below. The handshake is done already; the server's own is idempotent.
	//
	// Serve returns when Accept fails and at no other time, so the connection it is handed closes
	// the listener as well: its per-connection goroutine closes that connection once the last
	// request on it is done, Accept comes back with net.ErrClosed, and Serve returns. Closing the
	// connection alone is not enough - the defer above closes the connection, not the listener -
	// and without the listener going too this goroutine parks in Accept for the life of the
	// daemon, one per terminated tunnel.
	_ = http.Serve(ln, originForm(filter, host, port, inside.ConnectionState().ServerName))
}

// originForm turns each request inside the tunnel back into the absolute-form request the filter
// already knows how to handle, and hands it over - after checking that the three names for the
// destination agree.
//
// A request on a terminated tunnel carries an origin-form target (`GET /v1/messages`) and the
// destination only in its authority, because the client believes it is talking to the destination.
// The filter's relay path reads the URL, so the scheme and host it was CONNECTed to are put back -
// https, because this is a tunnel that was terminated, whatever the authority claims.
func originForm(filter http.Handler, host, port, sni string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL == nil {
			r.URL = &url.URL{}
		}

		if why := misdirected(host, port, sni, r); why != "" {
			http.Error(w, why, http.StatusMisdirectedRequest)

			return
		}

		r.URL.Scheme = "https"

		// The CONNECT target, not the request's authority: the authority is the client's claim
		// and the target is what the policy was checked against and what the certificate was
		// minted for. They have been proved equal just above, so this is now the same value said
		// once rather than a rewrite that hides a disagreement.
		r.URL.Host = net.JoinHostPort(host, port)
		r.Host = r.URL.Host

		// Set by http.Serve from the connection, and meaningless to the filter's relay, which
		// dials the upstream itself. Left in place it would make the request look like one that
		// arrived over TLS at the filter rather than one going out.
		r.TLS = nil
		r.RequestURI = ""

		// The relay leg is HTTP/1.1 whatever arrived here: the filter's transport writes an
		// HTTP/1.1 request, so a struct still saying HTTP/2.0 describes a wire nobody wrote. It
		// does not follow redirects either way - the relay is a RoundTrip, not a Client - so a
		// 3xx goes back to the agent, which is the only place that may decide to follow it.
		r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/1.1", 1, 1

		filter.ServeHTTP(w, r)
	})
}

// misdirected reports why a request inside the tunnel must not be served, or "" when the three
// names for its destination agree.
//
// There are three names, and a credential is added only when all of them are the same one: the
// CONNECT target the policy was checked against and the leaf was minted for, the SNI the client
// asked the handshake for, and the authority the request itself carries. HTTP/2 is what makes this
// worth enforcing rather than assuming. A client may coalesce requests for several authorities
// onto one connection it opened for a different one, so the authority of request two on an h2
// stream is not the name anything upstream of here was checked against - and the earlier code
// overwrote the authority with the CONNECT target, which turned exactly that disagreement into a
// request credentialled for a host the client never named.
//
// 421 and not 403, because it is the status that says this: the connection is wrong for this
// request, open another one. Go's own HTTP/2 client re-dials on a 421 rather than failing, so a
// coalescing client recovers by itself.
func misdirected(host, port, sni string, r *http.Request) string {
	// An authority with no port keeps the connection's: a client naming :443 and a client naming
	// nothing are talking about the same place. One that names a DIFFERENT port is not.
	authority, authorityPort, err := splitHostPort(r.Host)
	if err != nil {
		authorityPort = port
	}

	switch {
	case !sameHost(authority, host) || authorityPort != port:
		return "this request is for " + r.Host + " on a connection opened for " +
			net.JoinHostPort(host, port) + ", so no credential was added and nothing was sent. " +
			"A credential binding applies to the host the tunnel was opened to; open one per host"

	// An empty SNI is a client that connected without naming the host - by address, or with the
	// extension left off. It cannot agree with anything, so it does not: a vault whose leaves are
	// issued for names has nothing to check such a request against.
	case sni == "":
		return "this request arrived with no server name in its TLS handshake, so the name it " +
			"was sent to cannot be shown to be " + host + " and no credential was added. Connect " +
			"by hostname, not by address"

	case !sameHost(sni, host):
		return "this request's TLS handshake asked for " + sni + " on a connection opened for " +
			host + ", so no credential was added and nothing was sent"
	}

	return ""
}

// sameHost compares two hostnames the way the vault's own matching does: case-insensitively, with
// the root's trailing dot off.
func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

// leafFor is the certificate this filter presents as host, from the sandbox's own authority.
func (v *Vault) leafFor(host string) (*tls.Certificate, error) {
	v.mu.RLock()
	ca := v.ca
	exists := v.exists
	v.mu.RUnlock()

	if !exists || ca == nil {
		return nil, ErrNoAuthority
	}

	return ca.Leaf(host)
}

// ErrNoAuthority is a vault with no certificate authority to mint with - a vault that was deleted
// between the Bound check and the handshake.
var ErrNoAuthority = errors.New("this sandbox's vault has no certificate authority")

// targetOf splits a CONNECT target into host and port, defaulting the port to 443 as tunnel does.
func targetOf(target string) (string, string) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return strings.TrimSuffix(target, ":"), "443"
	}

	return host, port
}

// closesListener is the raw connection with one addition: closing it closes the listener too.
//
// That is the only thing that ends http.Serve here, because http.Serve returns when Accept fails,
// Accept fails when the listener is closed, and the listener is a oneConn that nobody else ever
// reaches. The wrapper rather than a close in serveTerminated: the connection ends on http.Serve's
// own goroutine, whenever the last request on it is done, and that is the moment the listener has
// to go.
//
// It sits UNDER the TLS layer rather than over it. Over it, the thing http.Serve is handed is not a
// *tls.Conn, so net/http cannot read the handshake's ALPN result and every tunnel is HTTP/1.1
// however the handshake ended. Closing still arrives: a tls.Conn's Close closes what it was built
// on, which is this.
type closesListener struct {
	net.Conn

	listener io.Closer
}

// Close closes the connection first, so a listener that is already closed cannot leave Accept
// handing over a connection that is gone. Both halves are idempotent.
func (c closesListener) Close() error {
	err := c.Conn.Close()

	_ = c.listener.Close()

	return err
}

// oneConn is a net.Listener that yields one already-accepted connection and then blocks until it
// is closed, which is how http.Serve is pointed at a single stream.
//
// Close does not close the connection: serveTerminated owns it through its own defer, and closing
// it twice would race the copy still draining it. The other direction does hold - the connection
// this yields is built on a closesListener - and that is what lets http.Serve return.
type oneConn struct {
	conn net.Conn

	handed  sync.Once
	closing sync.Once
	done    chan struct{}
	init    sync.Once
}

func (l *oneConn) ready() chan struct{} {
	l.init.Do(func() { l.done = make(chan struct{}) })

	return l.done
}

// Accept hands over the connection the first time, and afterwards waits for Close rather than
// returning an error straight away - http.Serve treats an immediate error as a reason to give up
// on the listener, and would tear down the connection it has just been given.
func (l *oneConn) Accept() (net.Conn, error) {
	ready := l.ready()

	var c net.Conn

	l.handed.Do(func() { c = l.conn })

	if c != nil {
		return c, nil
	}

	<-ready

	return nil, net.ErrClosed
}

// Close is idempotent: http.Serve closes the listener on its way out, and serveTerminated's own
// defer can reach it too, and closing a closed channel panics.
func (l *oneConn) Close() error {
	ready := l.ready()

	l.closing.Do(func() { close(ready) })

	return nil
}

func (l *oneConn) Addr() net.Addr { return l.conn.LocalAddr() }
