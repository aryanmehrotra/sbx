package daemon

// Reaching a deployed sandbox through the one HTTP endpoint a platform gives you.
//
// The daemon's ports stay bound to loopback exactly as they always were. This adds one
// handler, off unless asked for, that carries a TCP stream over a WebSocket - so `sbx connect`
// on a laptop can present the *same* local port numbers and `psql` connects to a sandbox on
// another machine without knowing anything happened.
//
// Read docs/superpowers/specs/2026-08-16-sbx-connect-design.md before changing this. Two
// things there are load-bearing and neither is obvious from the code:
//
//   - A port number is not an identity. Slots are reassigned when a sandbox is recreated, and
//     a docker ref is a *name* that survives `rm` + `create`, so a tunnel addressing a bare
//     port can splice a client into a different service - or an empty new volume - and report
//     nothing. Every dial carries the instance it believes it is reaching.
//   - This is the only part of sbx reachable from off the machine. Everything it does is
//     therefore refused by default and enabled deliberately.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/selfstat"
)

const (
	// pingEvery keeps an idle tunnel alive through an L7 proxy that reaps quiet connections.
	pingEvery = 30 * time.Second

	// pongDeadline is how long a peer may go without answering before the connection is
	// treated as dead. A proxy that drops a connection without a FIN leaves a socket that
	// reads and writes forever and reaches nothing.
	pongDeadline = 90 * time.Second

	// relayChunk is how much is read from the sandbox at a time.
	relayChunk = 32 << 10

	// halfClose is the wire signal for "my write side is finished", carried as a zero-length
	// binary frame.
	//
	// TCP has a half-close and a WebSocket does not: its close is bidirectional, so a client
	// doing write, shutdown(SHUT_WR), read - nc -N, an HTTP client sending Connection: close,
	// a Go io.Copy pipeline - had its reply truncated to nothing and reported as a clean EOF.
	//
	// A zero-length binary frame is free to mean this: both relays only ever write a frame when
	// they read more than zero bytes, so one has never been sent by the data path. It needs no
	// new opcode, which matters because an L7 proxy in the middle understands ordinary binary
	// frames and may not understand a reserved one.
	//
	// Old peers stay correct in both directions. One that receives it writes zero bytes
	// upstream, which is a no-op, and carries on exactly as before; one that never sends it is
	// handled by the error path that was always there. The client asks first anyway - the fleet
	// response advertises support - so it is never sent to a deployment that would ignore it.
	halfCloseFrameLen = 0

	// upstreamWait is how long a connection will wait for a workload that is still starting.
	// A database opening its data directory takes seconds; a port that is wrong takes forever,
	// so this is bounded rather than patient.
	upstreamWait = 60 * time.Second
)

// ConnectOptions is what `sbx serve --connect-addr` was given.
type ConnectOptions struct {
	Addr  string
	Token string

	// BehindProxy is the operator saying "something in front of me terminates TLS".
	//
	// Named for the claim rather than for the risk. A PaaS terminates TLS and forwards plain
	// HTTP to the container, so a flag called --insecure-http would have to be passed by every
	// correct deployment and would stop meaning anything. This one is a statement the operator
	// can be right or wrong about, which is the honest shape: the bind address cannot tell you
	// whether a terminator is in front, so only they can.
	BehindProxy bool
}

// Connect serves the tunnel endpoint until ctx is done. It is only called when the operator
// asked for it.
func (d *daemon) Connect(opt ConnectOptions) (*http.Server, error) {
	if opt.Token == "" {
		return nil, errors.New("--connect-addr needs SBX_CONNECT_TOKEN set to a non-empty value: " +
			"this is the only part of sbx reachable from off the machine, and an unauthenticated " +
			"one would hand every sandbox to anyone who found the port")
	}

	if !opt.BehindProxy && !loopbackOnly(opt.Addr) {
		return nil, fmt.Errorf("--connect-addr %s is not loopback and --behind-proxy was not given: "+
			"the token and every byte through the tunnel would cross the network in the clear. "+
			"Pass --behind-proxy if something in front of this terminates TLS", opt.Addr)
	}

	mux := http.NewServeMux()

	// Unauthenticated on purpose: a platform's probe carries no credential, and this says
	// nothing about what exists. Same reasoning as console/'s health endpoints.
	//
	// Liveness, not readiness. It answers 200 whenever the process is up, including when there
	// is no container runtime behind it - because a probe that fails there would have the
	// platform restart a process whose problem a restart cannot fix, and the restart loop is
	// what hides the reason. The reason goes in the body and in /v1/fleet.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if d.startupErr != nil && len(d.fronted) == 0 {
			_, _ = fmt.Fprintf(w, "degraded: no container runtime\n\n%v\n", d.startupErr)

			return
		}

		_, _ = io.WriteString(w, "ok\n")
	})

	mux.HandleFunc("GET /v1/fleet", d.authed(opt.Token, d.fleetHandler))
	mux.HandleFunc("GET /v1/connect", d.authed(opt.Token, d.connectHandler))

	// Control, behind the same token. Off in front mode - each handler refuses where there is
	// no provider - and a deliberate widening of what the token is worth where there is one:
	// `sbx ui --connect` can now wake, sleep, re-limit, remove and read logs, not only read.
	// See control.go.
	mux.HandleFunc("POST /v1/control/wake", d.authed(opt.Token, d.controlWake))
	mux.HandleFunc("POST /v1/control/sleep", d.authed(opt.Token, d.controlSleep))
	mux.HandleFunc("POST /v1/control/limit", d.authed(opt.Token, d.controlLimit))
	mux.HandleFunc("POST /v1/control/remove", d.authed(opt.Token, d.controlRemove))
	mux.HandleFunc("GET /v1/control/logs", d.authed(opt.Token, d.controlLogs))

	return &http.Server{
		Addr:    opt.Addr,
		Handler: mux,
		// A public listener with no header timeout is a slowloris target. There is no
		// WriteTimeout: a tunnel outlives any value that would be safe for an ordinary
		// request, and the hijacked connection clears its deadlines anyway.
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}

// authed rejects anything without the right bearer token.
//
// The token is only ever read from the header. A browser can be made to open a WebSocket to
// any URL, but the handshake sends only cookies and the URL - it cannot set a custom header -
// so header-only auth is what makes cross-site abuse impossible. Accepting `?token=` would
// reopen exactly that hole, which is why it is refused rather than merely undocumented.
func (d *daemon) authed(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("token") {
			http.Error(w, "the token goes in the Authorization header, never the URL", http.StatusBadRequest)

			return
		}

		got, ok := bearer(r)

		// Checked before the comparison: subtle.ConstantTimeCompare("", "") reports equal, so
		// a missing header against an empty token would authenticate itself.
		if !ok || got == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		next(w, r)
	}
}

func bearer(r *http.Request) (string, bool) {
	const prefix = "Bearer "

	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return "", false
	}

	return h[len(prefix):], true
}

// fleetService is one line of the fleet, as a client needs it.
type fleetService struct {
	Sandbox  string `json:"sandbox"`
	Service  string `json:"service"`
	Ref      string `json:"ref"`
	Instance string `json:"instance"`
	Awake    bool   `json:"awake"`
	Ports    []int  `json:"ports"`

	// What it is using and what it is allowed, for `sbx ui --connect`. Both optional, both
	// absent unless the caller asked with ?stats=1 - see fleetHandler.
	//
	// Absent and zero are different answers, which is why these are pointers: a service using
	// no cpu and a service whose sample never arrived are indistinguishable once both are 0,
	// and this project shows the second as "n/a" rather than as a measurement it did not take.
	Usage  *fleetUsage  `json:"usage,omitempty"`
	Limits *fleetLimits `json:"limits,omitempty"`
}

// fleetUsage is provider.Usage on the wire, field for field.
//
// The counters rather than a percentage, deliberately. CPU here is cumulative time, and a rate
// is the difference between two samples divided by the time between them - so sending a
// percentage would mean the server picking the window, and every client inheriting whatever it
// chose. The dashboard already computes rates from consecutive samples for local sandboxes;
// handing it the same counters means a remote sandbox goes through the identical arithmetic
// rather than a second implementation that rounds differently.
type fleetUsage struct {
	CPUNanos    uint64 `json:"cpu_nanos"`
	SystemNanos uint64 `json:"system_nanos"`
	OnlineCPUs  int    `json:"online_cpus"`
	MemBytes    uint64 `json:"mem_bytes"`
	MemLimit    uint64 `json:"mem_limit"`
}

// fleetLimits is provider.Limits on the wire. Zero means uncapped, which is a real answer
// rather than a missing one - so an absent object means "this backend cannot say", and a
// present one full of zeroes means "nothing is capped".
type fleetLimits struct {
	NanoCPUs int64  `json:"nano_cpus"`
	MemBytes uint64 `json:"mem_bytes"`
}

// fleetHandler answers what this daemon is fronting. Read-only, and it carries nothing that is
// not already on a `sbx list` screen.
func (d *daemon) fleetHandler(w http.ResponseWriter, r *http.Request) {
	// 503 rather than an empty list. "No sandboxes" and "I cannot see any sandboxes" are
	// different answers, and a client that cannot tell them apart will report the first.
	if d.startupErr != nil && len(d.fronted) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":  "this sbx has no container runtime, so it is fronting nothing",
			"detail": d.startupErr.Error(),
			"hint": "sbx manages containers on the machine it runs on. Mount the docker socket " +
				"into this container, or run it in a cluster with deploy/activator.yaml. A " +
				"platform that gives only a port and env vars cannot host it.",
		})

		return
	}

	d.mu.Lock()

	out := make([]fleetService, 0, len(d.units)+len(d.fronted))

	// Fronted ports first: where they exist they are usually the whole point, and a reader
	// scanning the list should not have to look past a fleet to find them.
	for _, f := range d.fronted {
		out = append(out, fleetService{
			Sandbox: f.name, Service: "fronted", Ref: f.name,
			Instance: frontInstance, Awake: true, Ports: []int{f.port},
		})
	}

	for _, u := range d.units {
		ports := make([]int, 0, len(u.legs))
		for _, l := range u.legs {
			ports = append(ports, l.Listen)
		}

		u.mu.Lock()
		awake := u.awake
		u.mu.Unlock()

		out = append(out, fleetService{
			Sandbox: u.sandbox, Service: u.service, Ref: u.ref,
			Instance: u.instance, Awake: awake, Ports: ports,
		})
	}

	d.mu.Unlock()

	// Usage costs a round trip per container, so it is opt-in rather than part of every
	// answer. `sbx connect` wants a port map and asks four times a session; `sbx ui --connect`
	// wants a dashboard and asks every couple of seconds. Charging the first for the second is
	// how a listing that used to be instant becomes the slowest thing in the command.
	if r.URL.Query().Get("stats") != "" {
		d.addUsage(r.Context(), out)
	}

	w.Header().Set("Content-Type", "application/json")
	// halfClose tells a client it may signal a half-close on this tunnel. A deployment without
	// it would ignore the frame, so the client keeps the old behaviour against one.
	_ = json.NewEncoder(w).Encode(map[string]any{"services": out, "halfClose": true})
}

// addUsage fills in what each service is using and what it is allowed, where this provider can
// say. Both are optional capabilities, so a backend without them leaves the fields absent and
// the dashboard reads "n/a" rather than showing a zero it did not measure.
func (d *daemon) addUsage(ctx context.Context, out []fleetService) {
	// Fronted services have no provider behind them - the platform started the workload - so
	// there is nothing to ask docker. But sbx is running *inside* that container, and a process
	// can read its own cgroup, so it reports the container's own usage as the fronted service's.
	// In a one-container-per-service deployment that cgroup is the service; where a container
	// fronts several ports they each show the container total, which is honest - they share it.
	if self, ok := selfstat.Read(); ok {
		for i := range out {
			if out[i].Instance != frontInstance {
				continue
			}

			out[i].Usage = &fleetUsage{
				CPUNanos: self.CPUNanos, SystemNanos: self.SystemNanos,
				OnlineCPUs: self.OnlineCPUs, MemBytes: self.MemBytes, MemLimit: self.MemLimit,
			}

			if self.NanoCPULimit > 0 || self.MemLimit > 0 {
				out[i].Limits = &fleetLimits{NanoCPUs: self.NanoCPULimit, MemBytes: self.MemLimit}
			}
		}
	}

	if d.provider == nil {
		return
	}

	refs := make([]string, 0, len(out))

	for _, f := range out {
		if f.Instance != frontInstance {
			refs = append(refs, f.Ref)
		}
	}

	if meter, ok := d.provider.(provider.Meter); ok && len(refs) > 0 {
		// One unreadable ref does not fail the others, and a ref that is asleep is simply
		// absent - which is why this ignores the error rather than refusing to answer at all.
		if stats, err := meter.Stats(ctx, refs); err == nil {
			for i := range out {
				u, ok := stats[out[i].Ref]
				if !ok {
					continue
				}

				out[i].Usage = &fleetUsage{
					CPUNanos: u.CPUNanos, SystemNanos: u.SystemNanos, OnlineCPUs: u.OnlineCPUs,
					MemBytes: u.MemBytes, MemLimit: u.MemLimit,
				}
			}
		}
	}

	lim, ok := d.provider.(provider.Limiter)
	if !ok {
		return
	}

	for i := range out {
		if out[i].Instance == frontInstance {
			continue
		}

		l, err := lim.Limits(ctx, out[i].Ref)
		if err != nil {
			continue
		}

		out[i].Limits = &fleetLimits{NanoCPUs: l.NanoCPUs, MemBytes: l.MemBytes}
	}
}

// fronted is a port this process was told to carry, rather than one it discovered.
//
// `sbx serve --front 5432` exists for the shape where sbx is not managing anything: it is in a
// container beside the workload, on a platform that gives one HTTP port and no container
// runtime, and its whole job is to carry TCP to a process that is already listening on
// loopback. There is nothing to wake - the platform woke the container - and nothing to
// discover, so these ports bypass the provider entirely.
type fronted struct {
	name string
	port int

	// host is where the traffic actually goes. Loopback for the original case - sbx in a
	// container beside the workload - and a real address for the one that made this a field:
	// a managed platform whose database sits at a private IP the container can route to and
	// you cannot. Fronting it turns "reachable only from inside" into a local port.
	host string
}

// frontInstance identifies this process, so a tunnel does not survive the container being
// replaced. A fronted port has no container ID to use and does not need one: if this process
// restarted, whatever was behind the port restarted with it.
var frontInstance = newInstanceID()

func newInstanceID() string {
	var b [8]byte

	_, _ = rand.Read(b[:])

	return "front-" + hex.EncodeToString(b[:])
}

// lookup finds the unit fronting a port, and reports whether the instance still matches.
func (d *daemon) lookup(port int, instance string) (*unit, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if f, ok := d.fronted[port]; ok {
		if instance == "" || instance != frontInstance {
			return nil, errStale
		}

		return &unit{sandbox: f.name, service: "fronted", ref: f.name, instance: frontInstance}, nil
	}

	for _, u := range d.units {
		for _, l := range u.legs {
			if l.Listen != port {
				continue
			}

			// An empty instance from the client is not "any instance". A client that cannot
			// name what it expects is a client that cannot be told when it is wrong.
			if instance == "" || u.instance != instance {
				return nil, errStale
			}

			return u, nil
		}
	}

	return nil, errNoSuchPort
}

var (
	errNoSuchPort = errors.New("this daemon fronts no such port")
	errStale      = errors.New("that port now serves a different instance")
)

// connectHandler carries one TCP stream over one WebSocket.
func (d *daemon) connectHandler(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil {
		http.Error(w, "port must be a number", http.StatusBadRequest)

		return
	}

	u, err := d.lookup(port, r.URL.Query().Get("instance"))

	switch {
	case errors.Is(err, errNoSuchPort):
		// Never dialled. Without this check the handler is an open proxy into whatever the
		// deployment can reach, which is a much larger thing than a tunnel to a sandbox.
		http.Error(w, "no sandbox is fronted on that port", http.StatusForbidden)

		return
	case errors.Is(err, errStale):
		http.Error(w, "that port serves a different instance now - the sandbox was recreated; "+
			"restart sbx connect to pick up the new map", http.StatusConflict)

		return
	}

	ws, err := wsUpgrade(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	defer func() { _ = ws.close() }()

	// Dialling the daemon's own wake port rather than the workload: a sleeping sandbox wakes
	// here exactly as it would for a local client, because it is the same listener.
	up, err := dialUpstream(d.frontHost(port), port)
	if err != nil {
		logs.Default.Info("connect: upstream refused", "sandbox", u.sandbox, "service", u.service)

		return
	}

	defer func() { _ = up.Close() }()

	logs.Default.Info("connect: tunnelling", "sandbox", u.sandbox, "service", u.service)

	relay(ws, up)
}

// dialUpstream connects to the workload, waiting for it if it is still starting.
//
// The retry is for the fronted case. A platform wakes a container and routes to it as soon as
// the HTTP port answers, which sbx does immediately - while the database beside it is still
// opening its data directory. Without this the first connection through a freshly woken tunnel
// is refused, and the first connection through a tunnel is usually somebody finding out whether
// it works at all. Doing it here rather than in a shell also means it works on any base image:
// waiting for a port in POSIX sh needs tools a minimal image does not have.
func dialUpstream(host string, port int) (net.Conn, error) {
	if host == "" {
		host = "127.0.0.1"
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(upstreamWait)

	for {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			return conn, nil
		}

		if time.Now().After(deadline) {
			return nil, err
		}

		time.Sleep(250 * time.Millisecond)
	}
}

// relay splices a WebSocket and a TCP connection until either ends.
//
// Both directions and the keepalive are shut down by the same channel, so a peer that vanishes
// takes its goroutines and its socket with it. Getting this wrong is how a tunnel leaks a
// goroutine and a file descriptor per dropped connection, which on a proxy that reaps idle
// connections is every connection eventually.
func relay(ws *wsConn, up net.Conn) {
	done := make(chan struct{})

	var once sync.Once

	stop := func() {
		once.Do(func() {
			close(done)
			_ = up.Close()
			_ = ws.conn.Close()
		})
	}

	go ws.keepalive(pingEvery, pongDeadline, done)

	var wg sync.WaitGroup

	wg.Add(2)

	// Both directions have to finish before the tunnel goes, because either may end while the
	// other still has the answer to deliver. Whichever ends second closes it.
	var halves atomic.Int32

	finish := func() {
		if halves.Add(1) == 2 {
			stop()
		}
	}

	// client -> sandbox
	go func() {
		defer wg.Done()

		for {
			payload, err := ws.readFrame()
			if err != nil {
				stop() // the tunnel itself ended; there is no other half to wait for

				return
			}

			// The client's write side finished. See halfClose.
			if len(payload) == 0 {
				if t, ok := up.(*net.TCPConn); ok {
					_ = t.CloseWrite()
					finish()

					return
				}

				stop() // nothing to half-close, so this is the end

				return
			}

			if _, err := up.Write(payload); err != nil {
				stop()

				return
			}
		}
	}()

	// sandbox -> client
	go func() {
		defer wg.Done()

		buf := make([]byte, relayChunk)

		for {
			n, err := up.Read(buf)
			if n > 0 {
				if werr := ws.write(opBinary, buf[:n]); werr != nil {
					stop()

					return
				}
			}

			if err != nil {
				// The workload closed cleanly: pass that on as a half-close so the client's
				// reader sees EOF, rather than killing a direction that may still be carrying
				// the request.
				if errors.Is(err, io.EOF) {
					_ = ws.write(opBinary, nil)
					finish()

					return
				}

				stop()

				return
			}
		}
	}()

	wg.Wait()
	stop()
}

// loopbackOnly reports whether an address can only be reached from this machine.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	if host == "" {
		return false // ":8080" is every interface
	}

	ip := net.ParseIP(host)

	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// frontHost is where a fronted port's traffic goes, or "" for a port this daemon discovered
// (which is always its own wake listener on loopback).
func (d *daemon) frontHost(port int) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	if f, ok := d.fronted[port]; ok {
		return f.host
	}

	return ""
}
