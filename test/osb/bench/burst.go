package main

// ComputeSDK's Burst TTI, reproduced against an OpenSandbox server.
//
// Their harness (computesdk/benchmarks, METHODOLOGY.md) launches 100 sandboxes at once and times,
// per sandbox, the client-side interval from create() to the first successful
// runCommand('node -v'). The score is 0.6·s(median) + 0.25·s(p95) + 0.15·s(p99) with
// s(ms) = 100·(1 − ms/10000) clamped to 0..100, multiplied by the success rate. This does the
// same through the upstream Go SDK - CreateSandbox (POST, GET until Running, the execd endpoint,
// /ping) then RunCommand - so the number includes every round trip a real client makes.
//
// Modes are interleaved and alternated per round (CONTRIBUTING.md): each round runs every mode
// once, the order rotating, so neither always gets the engine the other just finished with.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

type burstMode struct {
	name string
	ext  map[string]string
}

type burstResult struct {
	tti  []time.Duration
	errs []string
	n    int
}

func runBurst(t target, image string, n, rounds int, modes []burstMode, settle func(), out io.Writer) {
	results := map[string]*burstResult{}
	for _, m := range modes {
		results[m.name] = &burstResult{}
	}

	for r := 0; r < rounds; r++ {
		for i := range modes {
			m := modes[(i+r)%len(modes)]

			settle()

			fmt.Fprintf(os.Stderr, "round %d/%d: %s, burst %d\n", r+1, rounds, m.name, n)
			tti, errs := burstOnce(t, image, n, m.ext)

			res := results[m.name]
			res.tti = append(res.tti, tti...)
			res.errs = append(res.errs, errs...)
			res.n += n

			sorted := append([]time.Duration(nil), tti...)
			sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })

			if len(sorted) > 0 {
				fmt.Fprintf(os.Stderr, "  %d/%d ok, median %s ms, p95 %s ms, max %s ms\n", len(sorted), n,
					ms(pct(sorted, 50)), ms(pct(sorted, 95)), ms(sorted[len(sorted)-1]))
			}

			if len(errs) > 0 {
				fmt.Fprintf(os.Stderr, "  %d failed, first: %s\n", len(errs), errs[0])
			}
		}
	}

	fmt.Fprintf(out, "\nBurst TTI: %d rounds x %d concurrent create -> runCommand('node -v'), image %s, "+
		"modes interleaved and rotated each round.\n\n", rounds, n, image)
	fmt.Fprintln(out, "| mode | sandboxes | ok | median ms | p95 ms | p99 ms | min ms | max ms | score |")
	fmt.Fprintln(out, "|---|---:|---:|---:|---:|---:|---:|---:|---:|")

	for _, m := range modes {
		res := results[m.name]
		xs := res.tti
		sort.Slice(xs, func(a, b int) bool { return xs[a] < xs[b] })

		if len(xs) == 0 {
			fmt.Fprintf(out, "| %s | %d | 0 | - | - | - | - | - | 0 |\n", m.name, res.n)
			continue
		}

		med, p95, p99 := pct(xs, 50), pct(xs, 95), pct(xs, 99)
		rate := float64(len(xs)) / float64(res.n)

		fmt.Fprintf(out, "| %s | %d | %d | %s | %s | %s | %s | %s | %.2f |\n", m.name, res.n, len(xs),
			ms(med), ms(p95), ms(p99), ms(xs[0]), ms(xs[len(xs)-1]), score(med, p95, p99, rate))
	}

	for _, m := range modes {
		if errs := results[m.name].errs; len(errs) > 0 {
			fmt.Fprintf(out, "\n%s: %d failures; first: %s\n", m.name, len(errs), errs[0])
		}
	}
}

// score is ComputeSDK's composite, as METHODOLOGY.md defines it.
func score(med, p95, p99 time.Duration, rate float64) float64 {
	s := func(d time.Duration) float64 {
		v := 100 * (1 - float64(d.Milliseconds())/10000)
		return max(0, min(100, v))
	}

	return (0.6*s(med) + 0.25*s(p95) + 0.15*s(p99)) * rate
}

// burstOnce launches n creates at once, behind a start gate so they really are concurrent, and
// deletes every sandbox afterwards whether or not it succeeded.
func burstOnce(t target, image string, n int, ext map[string]string) ([]time.Duration, []string) {
	// The split of each TTI into create() and the command, reported alongside so a regression
	// says which half moved.
	var creates, cmds []time.Duration

	var (
		mu   sync.Mutex
		tti  []time.Duration
		errs []string
		sbs  []*opensandbox.Sandbox
		wg   sync.WaitGroup
		gate = make(chan struct{})
	)

	for range n {
		wg.Add(1)

		go func() {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			<-gate

			start := time.Now()

			var created time.Duration

			sb, err := opensandbox.CreateSandbox(ctx, t.cfg, opensandbox.SandboxCreateOptions{
				Image:      image,
				Extensions: ext,
			})
			if err == nil {
				mu.Lock()
				sbs = append(sbs, sb)
				mu.Unlock()

				created = time.Since(start)

				err = nodeVersion(ctx, sb)
			}

			d := time.Since(start)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err.Error())
				return
			}

			tti = append(tti, d)
			creates = append(creates, created)
			cmds = append(cmds, d-created)
		}()
	}

	close(gate)
	wg.Wait()

	// Deleted concurrently but bounded: a hundred DELETEs at once is its own burst, and this
	// one is not what is being measured.
	sem := make(chan struct{}, 16)

	for _, sb := range sbs {
		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			kctx, kcancel := context.WithTimeout(context.Background(), time.Minute)
			defer kcancel()

			if err := sb.Kill(kctx); err != nil {
				fmt.Fprintf(os.Stderr, "bench: could not kill %s: %v\n", sb.ID(), err)
			}
		}()
	}

	wg.Wait()

	if len(creates) > 0 {
		sort.Slice(creates, func(a, b int) bool { return creates[a] < creates[b] })
		sort.Slice(cmds, func(a, b int) bool { return cmds[a] < cmds[b] })
		fmt.Fprintf(os.Stderr, "  split: create() median %s ms p95 %s ms; node -v median %s ms p95 %s ms\n",
			ms(pct(creates, 50)), ms(pct(creates, 95)), ms(pct(cmds, 50)), ms(pct(cmds, 95)))
	}

	return tti, errs
}

// nodeVersion is ComputeSDK's probe command, and it must actually have printed a version: a
// fast wrong answer is not a measurement.
func nodeVersion(ctx context.Context, sb *opensandbox.Sandbox) error {
	ex, err := sb.RunCommand(ctx, "node -v", nil)
	if err != nil {
		return err
	}

	if ex.Error != nil {
		return fmt.Errorf("node -v: %s: %s", ex.Error.Name, ex.Error.Value)
	}

	var b strings.Builder
	for _, o := range ex.Stdout {
		b.WriteString(o.Text)
	}

	if !strings.HasPrefix(strings.TrimSpace(b.String()), "v") {
		return fmt.Errorf("node -v printed %q, not a version", b.String())
	}

	return nil
}

// awaitPool polls sbx's pool status until every pool is full and nothing is being made, for up
// to five minutes. A server without the route (not sbx, or no pools) returns at once.
func awaitPool(t target) {
	deadline := time.Now().Add(5 * time.Minute)
	url := t.cfg.GetBaseURL()
	url = strings.TrimSuffix(strings.TrimSuffix(url, "/"), "/v1") + "/sbx/v1/pool"

	for {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("OPEN-SANDBOX-API-KEY", t.cfg.GetAPIKey())

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bench: pool status: %v\n", err)
			return
		}

		var st []struct {
			Image                string
			Size, Ready, Filling int
		}

		ok := resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&st) == nil
		resp.Body.Close()

		if !ok {
			return
		}

		full := true
		for _, p := range st {
			full = full && p.Ready >= p.Size && p.Filling == 0
		}

		if full {
			return
		}

		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "bench: pools still not full after 5m: %+v\n", st)
			return
		}

		time.Sleep(500 * time.Millisecond)
	}
}
