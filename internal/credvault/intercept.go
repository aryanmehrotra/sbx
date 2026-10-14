package credvault

import (
	"crypto/tls"
	"errors"
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
func serveTerminated(client net.Conn, leaf *tls.Certificate, filter http.Handler, host, port string) {
	inside := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
	})
	defer inside.Close()

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
	// bodies and correct response framing, none of which is worth reimplementing here. It serves
	// exactly one connection and returns when that connection ends.
	_ = http.Serve(&oneConn{conn: inside}, originForm(filter, host, port))
}

// originForm turns each request inside the tunnel back into the absolute-form request the filter
// already knows how to handle, and hands it over.
//
// A request on a terminated tunnel carries an origin-form target (`GET /v1/messages`) and the
// destination only in Host, because the client believes it is talking to the destination. The
// filter's relay path reads the URL, so the scheme and host it was CONNECTed to are put back -
// https, because this is a tunnel that was terminated, whatever the Host header claims.
func originForm(filter http.Handler, host, port string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL == nil {
			r.URL = &url.URL{}
		}

		r.URL.Scheme = "https"

		// The CONNECT target, not the Host header: the header is the client's claim and the
		// target is what the policy was checked against and what the certificate was minted for.
		// A client that sent a different Host would otherwise pick its own destination.
		r.URL.Host = net.JoinHostPort(host, port)
		r.Host = r.URL.Host

		// Set by http.Serve from the connection, and meaningless to the filter's relay, which
		// dials the upstream itself. Left in place it would make the request look like one that
		// arrived over TLS at the filter rather than one going out.
		r.TLS = nil
		r.RequestURI = ""

		filter.ServeHTTP(w, r)
	})
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

// oneConn is a net.Listener that yields one already-accepted connection and then blocks until it
// is closed, which is how http.Serve is pointed at a single stream.
//
// Close does not close the connection: serveTerminated owns it through its own defer, and closing
// it twice would race the copy still draining it.
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
