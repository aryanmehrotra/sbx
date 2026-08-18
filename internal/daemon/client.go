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

// Endpoint is one deployment to reach. Several of them is the ordinary case: a sandbox is a
// group of services, and a platform that gives one container per service spreads that group
// over several deployments. Merging them here rather than cramming the services into one
// container is what keeps each service the image the spec actually named.
type Endpoint struct {
	Label string // as the user named it; empty means take its host
	URL   string
	Token string
}

// ClientOptions is what `sbx connect` was asked for.
type ClientOptions struct {
	Endpoints []Endpoint
	Sandbox   []string       // only these, if any
	Offset    int            // added to every local port
	Offsets   map[string]int // ... except these deployments, which get their own
	Out       io.Writer
}

// source is a resolved Endpoint: parsed, checked, and shared by every port it carries.
type source struct {
	label string
	base  *url.URL
	token string
	shift int
	found int // services it was fronting, before --sandbox took any of them away
}

// placed is one remote service and the deployment it came from.
type placed struct {
	svc fleetService
	src *source
}

// Connect opens the local listeners and serves them until ctx is done.
func Connect(ctx context.Context, opt ClientOptions) error {
	sources, err := resolve(opt)
	if err != nil {
		return err
	}

	fleets, err := fetchAll(ctx, sources)
	if err != nil {
		return err
	}

	var wanted []placed

	for i, src := range sources {
		// Kept before the filter runs, because "it has nothing" and "the filter took what it
		// had" are different things to tell somebody, and only one of them is --sandbox's fault.
		src.found = len(fleets[i])

		for _, svc := range chooseServices(fleets[i], opt.Sandbox) {
			wanted = append(wanted, placed{svc: svc, src: src})
		}
	}

	if len(wanted) == 0 {
		// One reason per deployment rather than one sentence for all of them. Joining the names
		// and picking a single reason means that whenever any of them was filtered, every other
		// one is told the filter was why - including the ones that were empty to begin with and
		// would say the same thing with no --sandbox at all. Which deployment is empty for which
		// reason is the useful part, and it is only per deployment that it can be true.
		var b strings.Builder

		b.WriteString("nothing to connect to:")

		for _, src := range sources {
			why := "is fronting nothing"
			if len(opt.Sandbox) > 0 && src.found > 0 {
				why = "has nothing matching --sandbox " + strings.Join(opt.Sandbox, ", ")
			}

			fmt.Fprintf(&b, "\n     %s %s", src.label, why)
		}

		return errors.New(b.String())
	}

	// Every listener is opened before any is served, so a clash is reported before the client
	// has half a tunnel. Aborting rather than skipping is deliberate: a skipped port leaves
	// this laptop's own daemon answering on it, and an agent would reach a local sandbox while
	// believing it reached the remote one.
	listeners, err := bindAll(wanted)
	if err != nil {
		return err
	}

	out := opt.Out
	if out == nil {
		out = os.Stdout
	}

	report(out, wanted, sources, len(opt.Sandbox) > 0)

	var wg sync.WaitGroup

	for _, l := range listeners {
		wg.Add(1)

		go func(l boundPort) {
			defer wg.Done()

			serveLocal(ctx, l)
		}(l)
	}

	<-ctx.Done()

	for _, l := range listeners {
		_ = l.ln.Close()
	}

	wg.Wait()

	return nil
}

// resolve turns what the command line said into something that can be dialled.
//
// The offset is applied here because this is where a deployment's label is finally known: the
// user may have named it, and if they did not it is the host. --port-offset db=1000 has to
// mean the same thing either way.
func resolve(opt ClientOptions) ([]*source, error) {
	eps := opt.Endpoints
	if len(eps) == 0 {
		return nil, errors.New("no deployment given: sbx connect <url> [<url> ...]")
	}

	out := make([]*source, 0, len(eps))
	seen := map[string]bool{}
	vars := map[string]string{}
	unused := map[string]bool{}

	for l := range opt.Offsets {
		unused[l] = true
	}

	for _, e := range eps {
		base, err := url.Parse(strings.TrimSuffix(e.URL, "/"))
		if err != nil || base.Host == "" {
			return nil, fmt.Errorf("%q is not a URL of a deployment: expected something like "+
				"https://sbx.example.dev", e.URL)
		}

		if err := insecureURL(base); err != nil {
			return nil, err
		}

		label := e.Label
		if label == "" {
			label = base.Host
		}

		if seen[label] {
			return nil, fmt.Errorf("two deployments are both called %q - name them apart, "+
				"as in db=%s", label, e.URL)
		}

		seen[label] = true

		// Two labels that are different to a person can be one environment variable: a name is
		// upper-cased and everything outside A-Z0-9 becomes an underscore, so db-1 and db_1 both
		// read SBX_CONNECT_TOKEN_DB_1. Left alone, the second deployment silently borrows the
		// first one's token - which is the exact thing naming them was supposed to avoid, failing
		// later as a rejected token on whichever one did not own it.
		if e.Label != "" {
			v := TokenVar(e.Label)

			if first, clash := vars[v]; clash {
				return nil, fmt.Errorf("%q and %q are different names for one variable (%s), "+
					"so both would read the same token - name them further apart",
					first, e.Label, v)
			}

			vars[v] = e.Label
		}

		if e.Token == "" {
			// Named endpoints get their own variable, because two deployments usually have two
			// tokens and sharing one would mean either reusing a secret or not connecting.
			hint := "set SBX_CONNECT_TOKEN to the value that deployment was given"
			if e.Label != "" {
				hint = fmt.Sprintf("set %s, or SBX_CONNECT_TOKEN for every deployment at once",
					TokenVar(e.Label))
			}

			return nil, fmt.Errorf("no token for %s: %s", label, hint)
		}

		shift := opt.Offset
		if n, ok := opt.Offsets[label]; ok {
			shift = n
			delete(unused, label)
		}

		out = append(out, &source{label: label, base: base, token: e.Token, shift: shift})
	}

	// A --port-offset naming a deployment that is not here is a typo, and silently ignoring it
	// means connecting on the ports the user was trying to move away from.
	if len(unused) > 0 {
		names := make([]string, 0, len(unused))
		for l := range unused {
			names = append(names, l)
		}

		sort.Strings(names)

		return nil, fmt.Errorf("--port-offset names %s, which is not one of the deployments given (%s)",
			strings.Join(names, ", "), strings.Join(labels(out), ", "))
	}

	return out, nil
}

// insecureURL refuses to send the token in the clear to somewhere off this machine.
//
// The server half already refuses to serve a connect endpoint on a non-loopback address without
// --behind-proxy, for the reason that the token and every byte through the tunnel would cross
// the network unencrypted. The client had no matching rule, so a mistyped or http:// URL sent
// the bearer token - the whole of the security - to whatever was on the other end. Loopback is
// exempt because that is what a port-forward, a test and a local daemon all look like.
//
// SBX_CONNECT_INSECURE=1 is the way out, for a network somebody has decided to trust. An
// environment variable rather than a flag: it is a property of where you are, not of the
// command, and it should be awkward enough to be deliberate.
func insecureURL(base *url.URL) error {
	if base.Scheme == "https" || os.Getenv("SBX_CONNECT_INSECURE") != "" {
		return nil
	}

	// EqualFold because a hostname is case-insensitive, and refusing http://LOCALHOST while
	// allowing http://localhost is a distinction nobody typing it believes in.
	if host := base.Hostname(); strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback() {
		return nil
	}

	// A URL with no scheme at all is not http, and saying it is sends somebody looking for an
	// s to add to a word that is not there.
	what := "is http"
	if base.Scheme == "" {
		what = "has no scheme"
	}

	return fmt.Errorf("%s %s, so SBX_CONNECT_TOKEN would cross the network in the clear\n"+
		"     use https, or set SBX_CONNECT_INSECURE=1 if you have decided this network is safe",
		base.Redacted(), what)
}

// TokenVar is the environment variable holding a named deployment's token.
func TokenVar(label string) string {
	var b strings.Builder

	b.WriteString("SBX_CONNECT_TOKEN_")

	for _, r := range strings.ToUpper(label) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)

			continue
		}

		b.WriteByte('_')
	}

	return b.String()
}

// SplitEndpoint reads a `label=url` argument, returning an empty label for a bare URL.
//
// A URL's scheme is followed by `://`, so anything with an `=` before any `:` or `/` was meant
// as a name. Guessing the other way round - treating `https://x` as a label - would turn a
// plain URL into a deployment nobody can address.
func SplitEndpoint(arg string) (label, rawURL string) {
	i := strings.Index(arg, "=")
	if i <= 0 {
		return "", arg
	}

	if strings.ContainsAny(arg[:i], ":/") {
		return "", arg
	}

	return arg[:i], arg[i+1:]
}

// ParseOffsets reads --port-offset: a number for every deployment, label=N for named ones, or
// both - `1000,replica=2000` is "move everything by 1000, except replica".
//
// Both, because the alternative is that naming one deployment's offset silently resets every
// other deployment's to zero. Somebody moving one service out of a clash would find the rest
// had quietly moved back onto the ports they were avoiding.
func ParseOffsets(spec string) (int, map[string]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, nil, nil
	}

	var (
		every    int
		haveHalf bool
		byLabel  map[string]int
	)

	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		name, num, named := strings.Cut(part, "=")

		if !named {
			n, err := strconv.Atoi(part)
			if err != nil {
				return 0, nil, fmt.Errorf("--port-offset %q: %q is neither a number nor label=number",
					spec, part)
			}

			if haveHalf {
				return 0, nil, fmt.Errorf("--port-offset %q: two offsets for everything - only "+
					"one can apply to the deployments you did not name", spec)
			}

			every, haveHalf = n, true

			continue
		}

		n, err := strconv.Atoi(strings.TrimSpace(num))
		if err != nil {
			return 0, nil, fmt.Errorf("--port-offset %q: %q is not a number", spec, num)
		}

		name = strings.TrimSpace(name)
		if name == "" {
			return 0, nil, fmt.Errorf("--port-offset %q: a number needs a deployment to apply to", spec)
		}

		if byLabel == nil {
			byLabel = map[string]int{}
		}

		if was, twice := byLabel[name]; twice {
			return 0, nil, fmt.Errorf("--port-offset %q: %s is given twice, as %d and %d",
				spec, name, was, n)
		}

		byLabel[name] = n
	}

	return every, byLabel, nil
}

func labels(sources []*source) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.label)
	}

	return out
}

// fetchAll asks every deployment what it is fronting, at the same time.
//
// At the same time because these wake on the first request: a platform that scales to zero
// takes seconds to answer the first one, and asking three in turn would cost that three times
// over for no reason. One failure fails the whole command - a half-built port map is the case
// where somebody reaches their own laptop's sandbox believing they reached the remote one.
func fetchAll(ctx context.Context, sources []*source) ([][]fleetService, error) {
	out := make([][]fleetService, len(sources))
	errs := make([]error, len(sources))

	var wg sync.WaitGroup

	for i, s := range sources {
		wg.Add(1)

		go func(i int, s *source) {
			defer wg.Done()

			out[i], errs[i] = fetchFleet(ctx, s.base, s.token, false)
		}(i, s)
	}

	wg.Wait()

	return out, errors.Join(errs...)
}

type boundPort struct {
	ln       net.Listener
	remote   int
	local    int
	instance string
	name     string
	src      *source
}

// The bounds on the one request that faces the open internet.
//
// http.DefaultClient has no timeout at all, so a deployment that accepts and then says nothing
// wedges the whole command with no output - and because every fleet is awaited before anything
// is printed, one black hole takes the rest down with it. Sixty seconds matches the docker
// client's own limit and leaves room for a platform that is cold-starting the container to
// answer the first request.
//
// The reply is a list of services and their ports, which is small. Decoding it unbounded means
// a wrong or hostile URL can spend this process's memory instead.
const maxFleetBody = 1 << 20

var fleetClient = &http.Client{Timeout: 60 * time.Second}

// stats asks the deployment to sample what each service is using, which costs it a round trip
// per running container. `sbx connect` does not want that - it wants a port map, once - so it
// passes false and the reply is exactly what it always was.
func fetchFleet(ctx context.Context, base *url.URL, token string, stats bool) ([]fleetService, error) {
	url := base.String() + "/v1/fleet"
	if stats {
		url += "?stats=1"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := fleetClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", base.Redacted(), err)
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%s rejected the token", base.Redacted())
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s asking what it is fronting", base.Redacted(), resp.Status)
	}

	var body struct {
		Services []fleetService `json:"services"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFleetBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("could not read what %s is fronting: %w", base.Redacted(), err)
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
func bindAll(services []placed) ([]boundPort, error) {
	var out []boundPort

	fail := func(err error) ([]boundPort, error) {
		for _, b := range out {
			_ = b.ln.Close()
		}

		return nil, err
	}

	// Which deployment already claimed a local port. Two of them fronting 5432 - two postgres,
	// which is a normal thing to want - would otherwise be a bind error naming the second one
	// only, reading as "something else has this port" when the something else is us.
	taken := map[int]placed{}

	for _, p := range services {
		for _, remote := range p.svc.Ports {
			local := remote + p.src.shift

			// Checked rather than left to net.Listen, which does not treat these as errors in
			// the way anybody expects: port 0 means "give me any free port", so an offset that
			// lands on it binds successfully to something random while the listing still says
			// :0 - a service that is quietly unreachable at the address it just printed. Out of
			// range does fail, but as "invalid port", which the bind-failure message below would
			// then blame on this machine's own daemon.
			if local < 1 || local > 65535 {
				return fail(fmt.Errorf("%s/%s is on %d, and %s's offset of %d puts it at %d, "+
					"which is not a port\n     an offset moves ports by a fixed amount; this one "+
					"has to keep them inside 1-65535",
					p.svc.Sandbox, p.svc.Service, remote, p.src.label, p.src.shift, local))
			}

			if first, clash := taken[local]; clash {
				return fail(fmt.Errorf("%s and %s both want 127.0.0.1:%d\n"+
					"     two deployments fronting the same port cannot share one local port.\n"+
					"     Move one of them: --port-offset %s=1000",
					first.src.label, p.src.label, local, p.src.label))
			}

			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
			if err != nil {
				return fail(fmt.Errorf("cannot open 127.0.0.1:%d for %s/%s: %w\n"+
					"     something already has it - often this machine's own `sbx serve`.\n"+
					"     Use --port-offset N to move every local port, --port-offset %s=N to move\n"+
					"     just this deployment's, or --sandbox to take fewer",
					local, p.svc.Sandbox, p.svc.Service, err, p.src.label))
			}

			taken[local] = p

			out = append(out, boundPort{
				ln: ln, remote: remote, local: local,
				instance: p.svc.Instance, name: p.svc.Sandbox + "/" + p.svc.Service,
				src: p.src,
			})
		}
	}

	return out, nil
}

func report(w io.Writer, services []placed, sources []*source, filtered bool) {
	sort.Slice(services, func(i, j int) bool {
		if services[i].svc.Sandbox != services[j].svc.Sandbox {
			return services[i].svc.Sandbox < services[j].svc.Sandbox
		}

		return services[i].svc.Service < services[j].svc.Service
	})

	one := len(sources) == 1

	if one {
		fmt.Fprintf(w, "sbx connect · %s\n\n", sources[0].base.Redacted())
	} else {
		fmt.Fprintf(w, "sbx connect · %d deployments\n", len(sources))
	}

	// Grouped by deployment rather than interleaved: the question somebody asks of this list is
	// "did the cache one come up", and an alphabetical merge buries the answer.
	for _, src := range sources {
		if !one {
			fmt.Fprintf(w, "\n  %s · %s\n", src.label, src.base.Redacted())
		}

		listed := 0

		for _, p := range services {
			if p.src != src {
				continue
			}

			for _, port := range p.svc.Ports {
				indent := "  "
				if !one {
					indent = "    "
				}

				listed++

				fmt.Fprintf(w, "%s127.0.0.1:%-6d %s/%s\n", indent, port+src.shift,
					p.svc.Sandbox, p.svc.Service)
			}
		}

		// A deployment with nothing under it otherwise reads as one that failed to come up. Why
		// it is empty is the useful half, and there are two different answers: a filter that
		// excluded everything it has, or a deployment that is fronting nothing at all. Naming
		// --sandbox in the second case would blame a flag the user never passed.
		if listed == 0 {
			// src.found, not the filter alone: a deployment that was fronting nothing in the
			// first place is empty whether or not --sandbox was passed, and blaming the flag
			// there sends somebody to check a filter that changed nothing for it.
			why := "fronting nothing"
			if filtered && src.found > 0 {
				why = "nothing here matches --sandbox"
			}

			fmt.Fprintf(w, "    %s\n", why)
		}
	}

	for _, src := range sources {
		if src.shift != 0 {
			fmt.Fprintf(w, "\n  %s is shifted by %d, so its own `sbx env` values do NOT apply here\n",
				src.label, src.shift)
		}
	}

	fmt.Fprintf(w, "\nOpening one of these wakes the sandbox behind it. Ctrl-C closes the tunnel.\n")
}

// serveLocal accepts on one local port and gives each connection its own tunnel.
func serveLocal(ctx context.Context, b boundPort) {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}

		go func() {
			defer c.Close()

			if err := tunnelOne(ctx, c, b, b.src.base, b.src.token); err != nil {
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
