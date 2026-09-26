package daemon

// Mirror is `sbx connect` that keeps up.
//
// Connect binds the ports a deployment is fronting at the moment it starts and serves exactly
// those until it is stopped: right for a person connecting to a deployment whose shape is
// settled. The Firecracker helper VM is not settled - sandboxes are created and removed in it
// while the Mac's `sbx serve --provider firecracker` is up - so a tunnel bound once would leave
// every sandbox created afterwards unreachable from the Mac, which is exactly the "connect to
// wake" promise broken.
//
// So this asks the same /v1/fleet on a tick and reconciles: a new port is bound, a gone one is
// closed, one whose instance changed (the in-VM daemon restarted) is rebound to carry the new
// id. Everything else - the fleet request, the loopback-only bind, the WebSocket tunnel per
// connection, the half-close negotiation - is Connect's own code, unchanged.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

var errTokenRejected = errors.New("rejected the token")

// MirrorOptions is one deployment to follow.
type MirrorOptions struct {
	Endpoint Endpoint
	Refresh  time.Duration // how often to ask what it is fronting; default 2s
	Shift    int           // added to every local port, as --port-offset
	Out      io.Writer

	// Sync asks for a reconcile now rather than at the next tick: send a channel, and it is
	// closed once the fleet has been asked and every port it lists is bound (or has failed to
	// bind, or the fleet could not be fetched). The helper VM's API front uses it so a create is
	// not answered before the new sandbox's endpoints listen here. Nil: ticks only.
	Sync <-chan chan struct{}
}

type mirrored struct {
	b    boundPort
	done chan struct{}
}

// Mirror serves until ctx ends. It returns an error only for what retrying cannot fix: a bad
// URL or a token the far side rejects. An unreachable daemon is retried, because the one in the
// helper VM is restarted by systemd and should be picked up again without anybody noticing.
func Mirror(ctx context.Context, opt MirrorOptions) error {
	sources, err := resolve(ClientOptions{Endpoints: []Endpoint{opt.Endpoint}, Offset: opt.Shift})
	if err != nil {
		return err
	}

	src := sources[0]

	out := opt.Out
	if out == nil {
		out = os.Stdout
	}

	refresh := opt.Refresh
	if refresh <= 0 {
		refresh = 2 * time.Second
	}

	bound := map[int]*mirrored{}
	failed := map[int]bool{}

	var wg sync.WaitGroup

	closeOne := func(local int) {
		m := bound[local]
		_ = m.b.ln.Close()
		<-m.done
		delete(bound, local)
	}

	defer func() {
		for local := range bound {
			closeOne(local)
		}

		wg.Wait()
	}()

	down := false
	tick := time.NewTicker(refresh)

	defer tick.Stop()

	// Sync requests waiting on the reconcile in progress. Released after every pass, and on
	// return, so a waiter never outlives the mirror.
	var waiting []chan struct{}

	release := func() {
		for _, c := range waiting {
			close(c)
		}

		waiting = nil
	}

	defer release()

	for {
		svcs, halfClose, err := fetchFleet(ctx, src.base, src.token, false)

		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, errTokenRejected):
			return err
		case err != nil:
			if !down {
				fmt.Fprintf(out, "sbx: %v - retrying every %s\n", err, refresh)
			}

			down = true
		default:
			if down {
				fmt.Fprintf(out, "sbx: %s is answering again\n", src.base.Redacted())
			}

			down = false
			src.halfClose.Store(halfClose)

			reconcile(ctx, out, src, svcs, bound, failed, closeOne, &wg)
		}

		release()

		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case c := <-opt.Sync:
			waiting = append(waiting, c)
		}
	}
}

type wantPort struct {
	remote   int
	instance string
	name     string
}

func reconcile(ctx context.Context, out io.Writer, src *source, svcs []fleetService,
	bound map[int]*mirrored, failed map[int]bool, closeOne func(int), wg *sync.WaitGroup,
) {
	want := map[int]wantPort{}

	for _, s := range svcs {
		for _, p := range s.Ports {
			local := p + src.shift
			if local < 1 || local > 65535 {
				continue
			}

			want[local] = wantPort{remote: p, instance: s.Instance, name: s.Sandbox + "/" + s.Service}
		}
	}

	for local, m := range bound {
		w, ok := want[local]
		if ok && w.instance == m.b.instance && w.remote == m.b.remote {
			continue
		}

		closeOne(local)

		if !ok {
			fmt.Fprintf(out, "  closed 127.0.0.1:%-6d %s\n", local, m.b.name)
		}
	}

	locals := make([]int, 0, len(want))
	for l := range want {
		locals = append(locals, l)
	}

	sort.Ints(locals)

	for _, local := range locals {
		if _, ok := bound[local]; ok {
			continue
		}

		w := want[local]

		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
		if err != nil {
			// Said once, retried every tick: the usual cause is this Mac's own docker `sbx
			// serve` fronting a sandbox on the same number, which may go away.
			if !failed[local] {
				fmt.Fprintf(out, "  cannot open 127.0.0.1:%d for %s: %v - something on this machine "+
					"already has it; retrying\n", local, w.name, err)
			}

			failed[local] = true

			continue
		}

		delete(failed, local)

		m := &mirrored{
			b: boundPort{ln: ln, remote: w.remote, local: local, instance: w.instance,
				name: w.name, src: src},
			done: make(chan struct{}),
		}
		bound[local] = m

		fmt.Fprintf(out, "  127.0.0.1:%-6d %s\n", local, w.name)

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer close(m.done)

			serveLocal(ctx, m.b)
		}()
	}

	for local := range failed {
		if _, ok := want[local]; !ok {
			delete(failed, local)
		}
	}
}
