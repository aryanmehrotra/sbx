package daemon

// The laptop half: local ports that reach a deployed sandbox.
//
//	sbx connect https://sbx.example.dev
//
// It opens a listener for every port the remote is fronting, on the SAME number, so a
// `sbx env` block from the deployment is literally correct here and `psql` connects to a
// database on another machine while knowing nothing about any of this. The socket is still the
// only signal; it just travels further.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ClientOptions is what `sbx connect` was asked for.
type ClientOptions struct {
	Base      string // https://host
	Token     string
	Sandbox   []string // only these, if any
	PortShift int      // added to every local port, for a laptop already running its own daemon
	Out       io.Writer
}

// Connect opens the local listeners and serves them until ctx is done.
func Connect(ctx context.Context, opt ClientOptions) error {
	if opt.Token == "" {
		return errors.New("no token: set SBX_CONNECT_TOKEN to the value the deployment was given")
	}

	base, err := url.Parse(strings.TrimSuffix(opt.Base, "/"))
	if err != nil || base.Host == "" {
		return fmt.Errorf("%q is not a URL of the deployment: expected something like https://sbx.example.dev", opt.Base)
	}

	fleet, err := fetchFleet(ctx, base, opt.Token)
	if err != nil {
		return err
	}

	wanted := chooseServices(fleet, opt.Sandbox)
	if len(wanted) == 0 {
		return errors.New("the deployment is fronting nothing that matches")
	}

	// Every listener is opened before any is served, so a clash is reported before the client
	// has half a tunnel. Aborting rather than skipping is deliberate: a skipped port leaves
	// this laptop's own daemon answering on it, and an agent would reach a local sandbox while
	// believing it reached the remote one.
	listeners, err := bindAll(wanted, opt.PortShift)
	if err != nil {
		return err
	}

	out := opt.Out
	if out == nil {
		out = os.Stdout
	}

	report(out, wanted, opt.PortShift, base)

	var wg sync.WaitGroup

	for _, l := range listeners {
		wg.Add(1)

		go func(l boundPort) {
			defer wg.Done()

			serveLocal(ctx, l, base, opt.Token)
		}(l)
	}

	<-ctx.Done()

	for _, l := range listeners {
		_ = l.ln.Close()
	}

	wg.Wait()

	return nil
}

type boundPort struct {
	ln       net.Listener
	remote   int
	local    int
	instance string
	name     string
}

func fetchFleet(ctx context.Context, base *url.URL, token string) ([]fleetService, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+"/v1/fleet", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", base, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%s rejected the token", base)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s asking what it is fronting", base, resp.Status)
	}

	var body struct {
		Services []fleetService `json:"services"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("could not read what %s is fronting: %w", base, err)
	}

	return body.Services, nil
}

func chooseServices(all []fleetService, only []string) []fleetService {
	if len(only) == 0 {
		return all
	}

	keep := map[string]bool{}
	for _, s := range only {
		keep[s] = true
	}

	out := make([]fleetService, 0, len(all))

	for _, s := range all {
		if keep[s.Sandbox] {
			out = append(out, s)
		}
	}

	return out
}

// bindAll opens every listener or none.
//
// Loopback only, never 0.0.0.0: the bearer token is checked once when this process dials the
// deployment, and the local listener re-authenticates nobody. On all interfaces it would hand
// anyone on the café wifi a fully authenticated tunnel into the deployment.
func bindAll(services []fleetService, shift int) ([]boundPort, error) {
	var out []boundPort

	fail := func(err error) ([]boundPort, error) {
		for _, b := range out {
			_ = b.ln.Close()
		}

		return nil, err
	}

	for _, s := range services {
		for _, remote := range s.Ports {
			local := remote + shift

			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
			if err != nil {
				return fail(fmt.Errorf("cannot open 127.0.0.1:%d for %s/%s: %w\n"+
					"     something already has it - often this machine's own `sbx serve`.\n"+
					"     Use --port-offset N to move every local port, or --sandbox to take fewer",
					local, s.Sandbox, s.Service, err))
			}

			out = append(out, boundPort{
				ln: ln, remote: remote, local: local,
				instance: s.Instance, name: s.Sandbox + "/" + s.Service,
			})
		}
	}

	return out, nil
}

func report(w io.Writer, services []fleetService, shift int, base *url.URL) {
	sort.Slice(services, func(i, j int) bool {
		if services[i].Sandbox != services[j].Sandbox {
			return services[i].Sandbox < services[j].Sandbox
		}

		return services[i].Service < services[j].Service
	})

	fmt.Fprintf(w, "sbx connect · %s\n\n", base)

	for _, s := range services {
		for _, p := range s.Ports {
			fmt.Fprintf(w, "  127.0.0.1:%-6d %s/%s\n", p+shift, s.Sandbox, s.Service)
		}
	}

	if shift != 0 {
		fmt.Fprintf(w, "\n  ports are shifted by %d, so the deployment's own `sbx env` values do NOT apply here\n", shift)
	}

	fmt.Fprintf(w, "\nOpening one of these wakes the sandbox behind it. Ctrl-C closes the tunnel.\n")
}

// serveLocal accepts on one local port and gives each connection its own tunnel.
func serveLocal(ctx context.Context, b boundPort, base *url.URL, token string) {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}

		go func() {
			defer c.Close()

			if err := tunnelOne(ctx, c, b, base, token); err != nil {
				fmt.Fprintf(os.Stderr, "sbx connect: %s: %v\n", b.name, err)
			}
		}()
	}
}

// tunnelOne carries one local TCP connection to the deployment.
func tunnelOne(ctx context.Context, local net.Conn, b boundPort, base *url.URL, token string) error {
	ws, err := dialConnect(ctx, base, token, b.remote, b.instance)
	if err != nil {
		return err
	}

	defer func() { _ = ws.close() }()

	relayClient(ws, local)

	return nil
}

// dialConnect performs the WebSocket handshake against the deployment.
func dialConnect(ctx context.Context, base *url.URL, token string, port int, instance string) (*wsConn, error) {
	host := base.Host
	if base.Port() == "" {
		if base.Scheme == "https" {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}

	conn, err := dialMaybeTLS(ctx, base.Scheme, host, base.Hostname())
	if err != nil {
		return nil, err
	}

	key := wsKey()

	req := fmt.Sprintf("GET /v1/connect?port=%d&instance=%s HTTP/1.1\r\n"+
		"Host: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n"+
		"Authorization: Bearer %s\r\n\r\n",
		port, url.QueryEscape(instance), base.Host, key, token)

	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()

		return nil, err
	}

	br := bufio.NewReader(conn)

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		_ = conn.Close()

		return nil, errors.New("the sandbox behind this port was recreated - restart sbx connect to pick up the new map")
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()

		return nil, fmt.Errorf("the deployment answered %s", resp.Status)
	}

	if resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		_ = conn.Close()

		return nil, errors.New("the handshake was answered by something that is not this endpoint")
	}

	return &wsConn{conn: conn, br: br, mask: true, lastPong: time.Now()}, nil
}

// relayClient is relay's mirror: local TCP in, server frames out.
func relayClient(ws *wsConn, local net.Conn) {
	done := make(chan struct{})

	var once sync.Once

	stop := func() {
		once.Do(func() {
			close(done)
			_ = local.Close()
			_ = ws.conn.Close()
		})
	}

	go ws.keepalive(pingEvery, pongDeadline, done)

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()
		defer stop()

		buf := make([]byte, relayChunk)

		for {
			n, err := local.Read(buf)
			if n > 0 {
				if err := ws.write(opBinary, buf[:n]); err != nil {
					return
				}
			}

			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer stop()

		for {
			payload, err := ws.readServerFrame()
			if err != nil {
				return
			}

			if _, err := local.Write(payload); err != nil {
				return
			}
		}
	}()

	wg.Wait()
	stop()
}

// dialMaybeTLS opens the transport the URL asked for.
func dialMaybeTLS(ctx context.Context, scheme, hostPort, serverName string) (net.Conn, error) {
	var d net.Dialer

	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", hostPort, err)
	}

	if scheme != "https" {
		return conn, nil
	}

	tc := tls.Client(conn, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12})

	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("TLS handshake with %s: %w", serverName, err)
	}

	return tc, nil
}
