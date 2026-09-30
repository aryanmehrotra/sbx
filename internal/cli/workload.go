package cli

// Whether the workload behind a service is serving, for `sbx ready`, `sbx wake` and `sbx create`.
//
// `sbx ready` used to stop at the daemon's own port. The daemon accepts there whether or not
// anything is behind it, so a sandbox whose workload could not be reached - Kata in nested
// colima with no guest network, a container whose network never came up - read as "is serving"
// and exited 0 while every real client got "Server closed the connection".
//
// Dialling the backing port from the host did not fix it, and was wrong both ways (see
// askInside). So a container's port is judged from inside the container: is something listening
// there where outside can reach it, and does the container have a network. The host dial is kept
// only where the container cannot be asked - a microVM, an image with no `cat` under gVisor or
// Kata (elsewhere a helper reads its tables: see helperTables), a runtime that does not say
// which port inside a backing port reaches - and it sends no protocol bytes: a
// server either speaks first (mysql, ssh) or waits for the client (redis, postgres, http), and a
// proxy with nothing behind it hangs up at once.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// workloadHold is how long a connection must stay open, silent, to count as a server waiting for
// its client. docker-proxy with nothing behind it closes within a few milliseconds of accepting;
// 300ms is two orders of magnitude of margin and still short enough to poll.
const workloadHold = 300 * time.Millisecond

// probeWorkload dials one workload port and reports whether a server is on the other end, and
// what was observed when it is not.
func probeWorkload(ctx context.Context, dial provider.DialFunc) (bool, string) {
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	c, err := dial(dctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return false, "the connection timed out"
		}

		if strings.Contains(err.Error(), "refused") {
			return false, "the connection was refused"
		}

		return false, "the connection failed: " + err.Error()
	}

	defer c.Close()

	_ = c.SetReadDeadline(time.Now().Add(workloadHold))

	var b [1]byte

	n, err := c.Read(b[:])

	switch {
	case n > 0:
		return true, "" // it spoke first
	case isTimeout(err):
		return true, "" // it is waiting for us
	case err == nil, errors.Is(err, io.EOF):
		return false, "it accepted the connection and closed it without a byte"
	default:
		return false, "it accepted the connection and closed it: " + err.Error()
	}
}

func isTimeout(err error) bool {
	var ne net.Error

	return errors.As(err, &ne) && ne.Timeout()
}

// workloadDial is one workload port and how to judge it.
type workloadDial struct {
	service string
	addr    provider.Endpoint
	dial    provider.DialFunc

	// inside reads the answer from inside the container (see askInside), or is nil where that
	// cannot be asked and the dial from the host decides alone.
	inside func(context.Context) insideAnswer
}

// workloadDials is every workload port of the local units, each with how to judge it.
//
// A port published on the host's loopback - docker's shape, runc, gVisor and Kata alike - is
// judged from inside its container. Everything else is dialled from the host, as the daemon
// dials it: a microVM's tap address or vsock (there is no forwarder between, to hold a line open
// for a guest that is not there), and any unit whose runtime does not say which port inside the
// container a backing port reaches.
//
// Remote units are left out for the same reason Ready leaves their daemon ports out: the address
// a cluster's activator dials is not one this machine can reach, or conclude anything from.
//
// since is when the command asking began: a container started at or after it that has exited
// since is a verdict waiting cannot change (see diedSince).
func workloadDials(p provider.Provider, units []provider.Unit, since time.Time) []workloadDial {
	gd, _ := p.(provider.GuestDialer)

	var out []workloadDial

	for _, u := range units {
		if !isLocal(u) {
			continue
		}

		for i, e := range u.Upstream {
			d := workloadDial{service: u.Service, addr: e}

			if gd != nil {
				if dial, ok := gd.GuestDialer(u.Sandbox, u.Service, e.Port); ok && dial != nil {
					d.dial = dial
				}
			}

			if d.dial == nil {
				addr := e.String()
				d.dial = func(ctx context.Context) (net.Conn, error) {
					var nd net.Dialer
					return nd.DialContext(ctx, "tcp", addr)
				}

				if e.Host == "127.0.0.1" && u.Ref != "" {
					sandbox, service, leg := u.Sandbox, u.Service, i
					d.inside = func(ctx context.Context) insideAnswer {
						return askInside(ctx, p, sandbox, service, leg, since)
					}
				}
			}

			out = append(out, d)
		}
	}

	return out
}

// insideAnswer is what a container said about one of its ports.
type insideAnswer struct {
	why      string // not serving, and why; "" when it is
	final    bool   // why is a verdict waiting cannot change: stop asking
	fallback bool   // cannot be asked this way at all: dial from the host instead
	err      error  // could not ask this time: ask again, never pass
}

// askInside reads two facts inside the container behind one published port, with nothing but
// `cat` (or, for an image without one, a helper in its network namespace) and no protocol bytes:
//
//   - is something LISTENING on the port inside the container, bound where outside can reach it
//     (a wildcard or a real address; a bind to 127.0.0.1 or ::1 serves only the container itself);
//   - does the container have a network interface besides loopback.
//
// Both, and it is serving. This replaced a dial from the host, which was wrong both ways: with
// Kata on a nested colima host the guest has no network and docker's forwarder holds a silent
// connection open for about 10 s (measured: `(sleep 10 | nc 127.0.0.1 30000)` held 9.6 s, three
// of three), which read as a server waiting for its client; and a real listener that accepts and
// closes without a byte (`nc -l </dev/null`) read as nothing there.
//
// Listed afresh rather than taken from the caller's units, because those were read before the
// wake and a stopped container publishes no inside port.
func askInside(ctx context.Context, p provider.Provider, sandbox, service string, leg int, since time.Time) insideAnswer {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return insideAnswer{err: err}
	}

	var u *provider.Unit

	for i := range units {
		if units[i].Service == service {
			u = &units[i]
		}
	}

	switch {
	case u == nil:
		return insideAnswer{err: fmt.Errorf("it is no longer listed")}
	case !u.Running:
		// Started during this command and exited since: nothing restarts it while this waits.
		if why, died := diedSince(ctx, p, u.Ref, since); died {
			return insideAnswer{why: why, final: true}
		}

		return insideAnswer{err: fmt.Errorf("its container is not running")}
	case leg >= len(u.Private) || u.Private[leg] == 0:
		return insideAnswer{fallback: true} // the runtime does not say which port to look for
	}

	port := u.Private[leg]

	cat := func(file string) (string, error) {
		return p.Exec(ctx, u.Ref, []string{"cat", file})
	}

	var dev, tcp, tcp6 string

	dev, err = cat("/proc/net/dev")

	switch {
	case err != nil && noTool(err):
		// No cat in the image. Read the same tables from a helper in its network namespace.
		t, ok := helperTables(ctx, p, *u)
		if !ok {
			return insideAnswer{fallback: true}
		}

		dev, tcp, tcp6 = t.Dev, t.TCP, t.TCP6
	case err != nil:
		return insideAnswer{err: err}
	default:
		if tcp, err = cat("/proc/net/tcp"); err != nil {
			return insideAnswer{err: err}
		}

		// A kernel with IPv6 off has no tcp6 table, which is not a failure to ask.
		if tcp6, err = cat("/proc/net/tcp6"); err != nil && !strings.Contains(err.Error(), "No such file") {
			return insideAnswer{err: err}
		}
	}

	ifaces := interfacesOf(dev)
	if len(ifaces) == 0 {
		return insideAnswer{err: fmt.Errorf("/proc/net/dev listed no interface at all")}
	}

	if !slices.ContainsFunc(ifaces, func(n string) bool { return n != "lo" }) {
		// Final: a container's interfaces are set up before its process starts - by docker for
		// runc and gVisor, by the Kata agent in the guest - so one with only loopback now will
		// have only loopback for as long as this waits. The RC4 retest waited out a full timeout
		// on it every time.
		return insideAnswer{why: "its container has no network interface but loopback, so nothing outside " +
			"can reach it (seen with Kata on a nested host)", final: true}
	}

	// A listener on 127.0.0.1 only is NOT final, though it is usually a config error. An image's
	// entrypoint may run its init against a private server bound there and then restart it on
	// every address, and calling that final fails a sandbox that would serve a moment later -
	// a wrong refusal, which costs a re-run, where a wrong wait costs only the timeout.
	switch reach, local := listenersOn(tcp+"\n"+tcp6, port); {
	case reach:
		return insideAnswer{}
	case local != "":
		return insideAnswer{why: fmt.Sprintf("it listens on %d only on %s inside the container, which nothing "+
			"outside it can connect to - bind 0.0.0.0 (or ::) instead", port, local)}
	default:
		why := fmt.Sprintf("nothing listens on %d inside the container", port)

		// The usual cause is a port number, not a process that is not running: say which.
		if others := reachableListeners(tcp + "\n" + tcp6); len(others) > 0 {
			why += fmt.Sprintf("; it listens on %s - declare that port in the spec, or make the "+
				"workload listen on %d", joinPorts(others), port)
		}

		return insideAnswer{why: why}
	}
}

// reachableListeners is every port something LISTENS on where outside the container can connect
// (not a loopback bind), sorted, once each.
func reachableListeners(tables string) []int {
	var out []int

	for _, line := range strings.Split(tables, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" {
			continue
		}

		addr, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}

		p, err := strconv.ParseUint(hexPort, 16, 16)
		if ip := procIP(addr); err != nil || ip == nil || ip.IsLoopback() {
			continue
		}

		if !slices.Contains(out, int(p)) {
			out = append(out, int(p))
		}
	}

	slices.Sort(out)

	return out
}

func joinPorts(ports []int) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strconv.Itoa(p)
	}

	return strings.Join(s, ", ")
}

// helperTables reads a unit's network tables through the provider's helper (NetTabler), for an
// image with no cat. Not ok - dial from the host instead - where the provider has no helper, where
// the helper could not run (no alpine:3 on an offline machine is not a verdict on the workload),
// and under gVisor or Kata: their guest kernel holds the workload's sockets, and the namespace a
// helper can join on the host would say "nothing listens" of a sandbox that serves.
func helperTables(ctx context.Context, p provider.Provider, u provider.Unit) (provider.NetTables, bool) {
	nt, ok := p.(provider.NetTabler)
	if !ok || (u.Isolation != "" && u.Isolation != provider.IsolationContainer) {
		return provider.NetTables{}, false
	}

	t, err := nt.NetTables(ctx, u.Ref)
	if err != nil {
		return provider.NetTables{}, false
	}

	return t, true
}

// noTool reports an exec that failed because the image has no such program: distroless, scratch.
// That never changes, so it is not "could not ask yet".
func noTool(err error) bool {
	for _, s := range []string{"executable file not found", "exit status 127", "exit status 126", "cat: not found"} {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}

	return false
}

// interfacesOf reads the interface names from /proc/net/dev: every "name:" line after the two
// header lines.
func interfacesOf(dev string) []string {
	var out []string

	for _, line := range strings.Split(dev, "\n") {
		name, _, ok := strings.Cut(line, ":")
		if !ok || strings.Contains(name, "|") {
			continue // a header line
		}

		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}

	return out
}

// listenersOn reads /proc/net/tcp and tcp6 rows for sockets LISTENING (state 0A) on port. reach
// is true when one is bound where outside the container can connect - a wildcard or a real
// address; local names a loopback address one is bound to, when that is all there is.
//
// Addresses are hex in the kernel's byte order, which is little-endian on every platform sbx
// runs containers on (amd64, arm64): each 32-bit word is reversed.
func listenersOn(tables string, port int) (reach bool, local string) {
	for _, line := range strings.Split(tables, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" {
			continue
		}

		addr, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}

		if p, err := strconv.ParseUint(hexPort, 16, 16); err != nil || int(p) != port {
			continue
		}

		ip := procIP(addr)
		if ip == nil {
			continue
		}

		if !ip.IsLoopback() {
			return true, ""
		}

		local = ip.String()
	}

	return false, local
}

// procIP decodes a /proc/net/tcp{,6} address: 8 or 32 hex digits, in little-endian 32-bit words.
func procIP(s string) net.IP {
	raw, err := hex.DecodeString(s)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil
	}

	for w := 0; w < len(raw); w += 4 {
		raw[w], raw[w+1], raw[w+2], raw[w+3] = raw[w+3], raw[w+2], raw[w+1], raw[w]
	}

	return net.IP(raw)
}

// waitWorkloads polls every workload port until each is serving, and names each one that is not
// when the deadline passes, with the reason. Like waitRunning it always looks at least once, so
// a deadline the health waits used up still gets an answer rather than a pass.
//
// It polls because a woken container is running before its process listens. A port that has
// passed is not asked again, and the ports of one poll are asked together: each is a few execs,
// and a 14-service stack asked one by one would spend seconds on every look.
//
// It stops before the deadline on a verdict waiting cannot change (judgeWorkload's final): that
// port will fail at the deadline too, so the wait would only delay the same answer. Ports still
// failing a verdict that can change are named as not yet serving, not as broken.
func waitWorkloads(ctx context.Context, sandbox string, dials []workloadDial, deadline time.Time) error {
	passed := make([]bool, len(dials))

	for {
		why := make([]string, len(dials))
		final := make([]bool, len(dials))

		var wg sync.WaitGroup

		for i, d := range dials {
			if passed[i] {
				continue
			}

			wg.Go(func() { why[i], final[i] = judgeWorkload(ctx, d) })
		}

		wg.Wait()

		var failed, pending []string

		for i, d := range dials {
			if passed[i] {
				continue
			}

			if why[i] == "" {
				passed[i] = true
				continue
			}

			line := fmt.Sprintf("\n     %s: %s\n       see why: sbx logs %s %s", d.service, why[i], sandbox, d.service)

			if final[i] {
				failed = append(failed, line)
			} else {
				pending = append(pending, line)
			}
		}

		switch {
		case len(failed) == 0 && len(pending) == 0:
			return nil
		case len(failed) > 0:
			for _, line := range pending {
				failed = append(failed, strings.Replace(line, ": ", ": not serving yet when this stopped - ", 1))
			}

			return fmt.Errorf("sandbox %q is not serving:%s", sandbox, strings.Join(failed, ""))
		case !time.Now().Before(deadline):
			return fmt.Errorf("sandbox %q is not serving:%s", sandbox, strings.Join(pending, ""))
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// judgeWorkload is one look at one port: "" when it is serving, otherwise why not, and whether
// that is final - a verdict no amount of waiting changes.
func judgeWorkload(ctx context.Context, d workloadDial) (string, bool) {
	if d.inside != nil {
		a := d.inside(ctx)

		switch {
		case a.err != nil:
			return "could not ask its container whether it is serving: " + firstLine(a.err.Error()), false
		case !a.fallback:
			return a.why, a.final
		}
	}

	// The dial from the host, where the container cannot be asked. It cannot tell a listener
	// that accepts and closes without a byte from nothing there, and on colima it cannot see
	// past a forwarder holding the line - which is why it is the fallback, not the check.
	if ok, observed := probeWorkload(ctx, d.dial); !ok {
		return fmt.Sprintf("at %s (where `sbx serve` forwards it): %s", d.addr, observed), false
	}

	return "", false
}

// diedSince is the runtime's account of a container that was started at or after since and is
// not running now, as one clause for an error - a verdict waiting cannot change, because nothing
// in the command asking will start it again. False where the runtime cannot say (no
// provider.ExitReporter, or it could not be asked), and for a container last started before
// since: that one is asleep, and the wake this command asked for may not have reached it yet.
func diedSince(ctx context.Context, p provider.Provider, ref string, since time.Time) (string, bool) {
	er, ok := p.(provider.ExitReporter)
	if !ok {
		return "", false
	}

	st, err := er.ExitOf(ctx, ref)
	if err != nil || st.Status == "running" || st.StartedAt.IsZero() || st.StartedAt.Before(since) {
		return "", false
	}

	return "its container is not running: " + st.String(), true
}

// checkCreatedWorkloads is the workload check for the services a create just made. Create used to
// print "ready" once each health check passed, and a health check runs inside the container - so a
// guest with no network (Kata in nested colima) passed it while every host connection was dropped.
//
// Only services this create made, and only those running now: a microVM's create ends by putting
// it to sleep, and its port has nothing behind it until a connection wakes it, which is not a
// failure. Asking is cheap for the rest, so this does not wait for anything to wake.
//
// Not running is a failure in one case: the runtime records that it started during this create
// (at or after began) and has exited since. That container outlived its tick and died before this
// check, which skipped it and printed "ready" for a sandbox whose service was gone. One asleep
// before this create - left as it is on a re-run - started earlier, and still passes.
func checkCreatedWorkloads(ctx context.Context, p provider.Provider, sandbox string, made []string,
	began, deadline time.Time,
) error {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	want := map[string]bool{}
	for _, s := range made {
		want[s] = true
	}

	var (
		check  []provider.Unit
		exited []string
	)

	for _, u := range units {
		switch {
		case !want[u.Service]:
		case u.Running:
			check = append(check, u)
		default:
			if why, died := diedSince(ctx, p, u.Ref, began); died {
				exited = append(exited, fmt.Sprintf("\n     %s: %s\n       see why: sbx logs %s %s",
					u.Service, why, sandbox, u.Service))
			}
		}
	}

	err = waitWorkloads(ctx, sandbox, workloadDials(p, check, began), deadline)

	switch {
	case len(exited) == 0:
		return err
	case err != nil:
		return fmt.Errorf("%w%s", err, strings.Join(exited, ""))
	default:
		return fmt.Errorf("sandbox %q is not serving:%s", sandbox, strings.Join(exited, ""))
	}
}
