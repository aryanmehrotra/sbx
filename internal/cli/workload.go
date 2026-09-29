package cli

// Whether the workload behind a service answers, asked at the address the daemon forwards to.
//
// `sbx ready` used to stop at the daemon's own port. The daemon accepts there whether or not
// anything is behind it, so a sandbox whose workload could not be reached - Kata in nested
// colima with no guest network, a container whose network never came up - read as "is serving"
// and exited 0 while every real client got "Server closed the connection".
//
// A published docker port is docker-proxy, and docker-proxy accepts before it knows whether the
// container will. So "the dial succeeded" is not the answer either: what separates a server from
// a proxy with nothing behind it is what happens NEXT. A server either speaks first (mysql, ssh)
// or waits for the client (redis, postgres, http); a proxy with nothing behind it hangs up at
// once. No protocol bytes are sent, because this has to work for every workload, and a byte
// that is valid for one is garbage to another.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
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

// workloadDial is one workload port and how to dial it.
type workloadDial struct {
	service string
	addr    provider.Endpoint
	dial    provider.DialFunc
}

// workloadDials is every workload port of the local units: over the provider's own dialer where
// it has one (a microVM's vsock), otherwise TCP to Upstream - the same choice the daemon makes.
//
// Remote units are left out for the same reason Ready leaves their daemon ports out: the address
// a cluster's activator dials is not one this machine can reach, or conclude anything from.
func workloadDials(p provider.Provider, units []provider.Unit) []workloadDial {
	gd, _ := p.(provider.GuestDialer)

	var out []workloadDial

	for _, u := range units {
		if !isLocal(u) {
			continue
		}

		for _, e := range u.Upstream {
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
			}

			out = append(out, d)
		}
	}

	return out
}

// waitWorkloads polls every workload port until each has a server behind it, and names each one
// that does not when the deadline passes. Like waitRunning it always looks at least once, so a
// deadline the health waits used up still gets an answer rather than a pass.
//
// It polls because a woken container is running before its process listens, and in that window
// docker-proxy answers exactly as it does for a workload that never will.
func waitWorkloads(ctx context.Context, sandbox string, dials []workloadDial, deadline time.Time) error {
	for {
		var failed []string

		for _, d := range dials {
			if ok, observed := probeWorkload(ctx, d.dial); !ok {
				failed = append(failed, fmt.Sprintf("\n     %s at %s (where `sbx serve` forwards it): %s"+
					"\n       see why: sbx logs %s %s", d.service, d.addr, observed, sandbox, d.service))
			}
		}

		if len(failed) == 0 {
			return nil
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("sandbox %q is not serving - the daemon accepts, but the workload behind it "+
				"does not answer:%s\n     A runtime whose guest has no network looks like this (seen with Kata in nested colima)", sandbox, strings.Join(failed, ""))
		}

		time.Sleep(200 * time.Millisecond)
	}
}
