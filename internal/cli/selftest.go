package cli

// Proof, on your machine, in about nine seconds once images are local.
//
//	sbx selftest
//	sbx selftest --provider kubernetes
//
// The honest weakness of this tool is that one person has run it. That cannot be fixed by
// writing more of it - only by somebody else running it - so the least this can do is make
// running it a single command that either proves the claim or says exactly which part of it
// failed.
//
// It asserts the four things the design actually promises:
//
//	1. a sandbox can be created from a spec
//	2. it goes to zero when nobody is using it
//	3. a plain TCP connection wakes it, with no API call
//	4. what was written before it slept is still there afterwards
//
// Nothing is stubbed. It is the real provider, the real daemon loop and a real client.

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// selftestSpec is deliberately Redis: small, fast, and its readiness check is unambiguous.
// The point is to exercise sbx, not to benchmark a database.
func selftestSpec() *spec.Spec {
	return &spec.Spec{
		Version: 1,
		Services: map[string]spec.Service{
			"redis": {
				Image:  "redis:7-alpine",
				Ports:  []int{6379},
				Health: "redis-cli ping",
			},
		},
	}
}

type step struct {
	name string
	took time.Duration
	err  error
}

func Selftest(ctx context.Context, p provider.Provider, iso provider.Isolation, keep bool) error {
	name := fmt.Sprintf("selftest-%d", os.Getpid())
	sp := selftestSpec()

	fmt.Printf("sbx selftest · provider %s · isolation %s · sandbox %q\n\n", p.Name(), iso, name)

	var steps []step

	record := func(what string, f func() error) bool {
		start := time.Now()
		err := f()
		steps = append(steps, step{what, time.Since(start), err})

		mark := "✓"
		if err != nil {
			mark = "✗"
		}

		fmt.Printf("  %s %-34s %6dms\n", mark, what, time.Since(start).Milliseconds())

		if err != nil {
			fmt.Printf("      %v\n", err)
		}

		return err == nil
	}

	// Cleanup is unconditional and runs even when an assertion fails: a self-test that
	// leaves debris behind teaches people not to run it.
	defer func() {
		if keep {
			fmt.Printf("\n  --keep: sandbox %q left in place\n", name)
			return
		}

		if err := p.Remove(ctx, name); err != nil {
			fmt.Fprintf(os.Stderr, "  cleanup: %v\n", err)
		}
	}()

	layout, err := sp.Assign()
	if err != nil {
		return err
	}

	slot, err := p.AllocSlot(ctx, name)
	if err != nil {
		return err
	}

	start, _ := sp.StartIndex(layout, "redis")

	if !record("create a sandbox from a spec", func() error {
		return createOne(ctx, p, name, slot, start, "redis", sp.Services["redis"], ".", iso)
	}) {
		return fmt.Errorf("selftest failed at create")
	}

	ref, err := refFor(ctx, p, name, "redis")
	if err != nil {
		return err
	}

	if !record("write a value", func() error {
		_, err := p.Exec(ctx, ref, []string{"redis-cli", "set", "selftest", "survived a sleep"})
		return err
	}) {
		return fmt.Errorf("selftest failed writing state")
	}

	// The daemon runs in this process for the duration. That is the honest way to test it:
	// the same discovery loop, the same wake path, the same reaper.
	units, err := p.List(ctx, name)
	if err != nil || len(units) == 0 {
		return fmt.Errorf("selftest: provider does not list the sandbox it just created")
	}

	d := daemon.New(p, 3*time.Second, 90*time.Second, time.Second)

	dctx, dcancel := context.WithCancel(ctx)
	defer dcancel()

	go d.Run(dctx)

	if !record("sleep to zero when unused", func() error {
		deadline := time.Now().Add(90 * time.Second)

		for time.Now().Before(deadline) {
			cur, err := p.List(ctx, name)
			if err == nil && len(cur) > 0 && !cur[0].Running {
				return nil
			}

			time.Sleep(500 * time.Millisecond)
		}

		return fmt.Errorf("still running after 90s - the reaper never slept it")
	}) {
		return fmt.Errorf("selftest failed at sleep")
	}

	// A plain socket, opened by nothing that knows what sbx is. This is the assertion the
	// hosted platforms cannot satisfy: no SDK, no resume call, just a connection.
	var got string

	if !record("wake on a plain TCP connection", func() error {
		wake := units[0].Client[0]

		conn, err := net.DialTimeout("tcp", wake.String(), 120*time.Second)
		if err != nil {
			return fmt.Errorf("dial %s: %w", wake, err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte("GET selftest\r\n")); err != nil {
			return err
		}

		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))

		buf := make([]byte, 128)

		n, err := conn.Read(buf)
		if err != nil {
			return fmt.Errorf("read after wake: %w", err)
		}

		got = string(buf[:n])

		return nil
	}) {
		return fmt.Errorf("selftest failed at wake")
	}

	if !record("state survived the sleep", func() error {
		// RESP: "$16\r\nsurvived a sleep\r\n". Checking the payload is the point; parsing
		// the protocol properly is not.
		if !strings.Contains(got, "survived a sleep") {
			return fmt.Errorf("expected the value written before sleeping, got %q", got)
		}

		return nil
	}) {
		return fmt.Errorf("selftest failed: state did not survive")
	}

	fmt.Printf("\n  all %d checks passed\n", len(steps))
	fmt.Println("  a sandbox was created, slept to zero, woken by a socket, and remembered.")

	return nil
}
