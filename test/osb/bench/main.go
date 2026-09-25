// Command bench measures an OpenSandbox lifecycle API through the upstream Go SDK, so sbx and
// a real OpenSandbox server are timed by the same client code doing the same things.
//
//	go run ./bench -target sbx=http://127.0.0.1:8080 [-target osb=http://host:8080] [-rounds 5]
//
// Keys come from the environment, never the command line: OSB_KEY_<NAME> (upper-cased target
// name), falling back to OPENSANDBOX_TEST_API_KEY.
//
// Two rules from CONTRIBUTING.md shape the loop. Interleave: every round runs every target,
// so no target gets a docker daemon the other has just finished hammering for a whole phase.
// Alternate: the order rotates each round, because going second is itself a bias - one large
// enough here, once, to reverse the sign of a result.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

type target struct {
	name string
	cfg  opensandbox.ConnectionConfig
}

type targets []target

func (t *targets) String() string { return fmt.Sprint(len(*t)) }

func (t *targets) Set(v string) error {
	name, raw, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("want name=http://host:port, got %q", v)
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("target %s: want http(s)://host:port, got %q", name, raw)
	}

	key := os.Getenv("OSB_KEY_" + strings.ToUpper(name))
	if key == "" {
		key = os.Getenv("OPENSANDBOX_TEST_API_KEY")
	}

	*t = append(*t, target{name: name, cfg: opensandbox.ConnectionConfig{
		Domain:         u.Host,
		Protocol:       u.Scheme,
		APIKey:         key,
		RequestTimeout: 3 * time.Minute,
	}})

	return nil
}

// The metrics, in the order they are taken inside one sandbox's life.
const (
	mCreate = "create → first command"
	mRTT    = "command round trip"
	mUp     = "upload 1 MiB"
	mDown   = "download 1 MiB"
	mResume = "pause → resume → first command"
	mIdle   = "wake from idle-frozen → first command"
)

type samples map[string]map[string][]time.Duration // metric -> target -> samples
type failures map[string]map[string][]string       // metric -> target -> errors

func (s samples) add(m, t string, d time.Duration) {
	if s[m] == nil {
		s[m] = map[string][]time.Duration{}
	}

	s[m][t] = append(s[m][t], d)
}

func (f failures) add(m, t string, err error) {
	if f[m] == nil {
		f[m] = map[string][]string{}
	}

	f[m][t] = append(f[m][t], err.Error())
}

type config struct {
	image    string
	rtt      int
	size     int
	idleWait time.Duration
}

func main() {
	var ts targets

	flag.Var(&ts, "target", "name=http(s)://host:port; repeatable, and the order rotates each round")
	rounds := flag.Int("rounds", 5, "sandboxes created per target")
	image := flag.String("image", "python:3.11-slim", "sandbox image (upstream's e2e default)")
	rtt := flag.Int("rtt", 10, "command round trips per sandbox")
	size := flag.Int("size", 1<<20, "bytes uploaded and downloaded")
	serverIdle := flag.Duration("server-idle", 0,
		"the server's idle window (sbx serve --idle); when set, each sandbox is left alone 5s past it "+
			"and then timed waking from the idle freeze. 0 skips the measurement")
	flag.Parse()

	// Past the window by a margin, so the reaper has certainly acted: the reaper ticks on a
	// fraction of the window, and a wake measured before the freeze is a round trip.
	idleWait := time.Duration(0)
	if *serverIdle > 0 {
		idleWait = *serverIdle + 5*time.Second
	}

	if len(ts) == 0 {
		fmt.Fprintln(os.Stderr, "bench: at least one -target name=http://host:port is required")
		os.Exit(2)
	}

	cfg := config{image: *image, rtt: *rtt, size: *size, idleWait: idleWait}
	s, f := samples{}, failures{}

	for r := 0; r < *rounds; r++ {
		for i := range ts {
			t := ts[(i+r)%len(ts)] // rotate: every target takes every position in turn
			fmt.Fprintf(os.Stderr, "round %d/%d: %s\n", r+1, *rounds, t.name)
			once(t, cfg, s, f)
		}
	}

	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.name)
	}

	ok := table(os.Stdout, names, s, f, cfg, *rounds)

	if !ok {
		fmt.Fprintln(os.Stderr, "bench: no metric produced a single sample - nothing was measured")
		os.Exit(1)
	}
}

// once is one sandbox's life: create it, time everything, kill it. Every error is recorded
// against its metric rather than aborting, so one broken endpoint does not hide the others.
func once(t target, cfg config, s samples, f failures) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	start := time.Now()

	sb, err := opensandbox.CreateSandbox(ctx, t.cfg, opensandbox.SandboxCreateOptions{Image: cfg.image})
	if err != nil {
		f.add(mCreate, t.name, err)
		return
	}

	defer func() {
		kctx, kcancel := context.WithTimeout(context.Background(), time.Minute)
		defer kcancel()

		if err := sb.Kill(kctx); err != nil {
			fmt.Fprintf(os.Stderr, "bench: could not kill %s on %s: %v\n", sb.ID(), t.name, err)
		}
	}()

	if err := command(ctx, sb, "true"); err != nil {
		f.add(mCreate, t.name, err)
		return
	}

	s.add(mCreate, t.name, time.Since(start))

	for i := 0; i < cfg.rtt; i++ {
		d, err := timed(func() error { return command(ctx, sb, "echo ok") })
		if err != nil {
			f.add(mRTT, t.name, err)
			continue
		}

		s.add(mRTT, t.name, d)
	}

	payload := make([]byte, cfg.size)
	_, _ = rand.Read(payload)

	const remote = "/tmp/sbx-osb-bench.bin"

	d, err := timed(func() error {
		return sb.UploadFile(ctx, bytes.NewReader(payload), opensandbox.UploadFileOptions{
			FileName: "sbx-osb-bench.bin",
			Metadata: opensandbox.FileMetadata{Path: remote},
		})
	})
	if err != nil {
		f.add(mUp, t.name, err)
	} else {
		s.add(mUp, t.name, d)

		d, err = timed(func() error {
			rc, err := sb.DownloadFile(ctx, remote, "")
			if err != nil {
				return err
			}
			defer rc.Close()

			got, err := io.ReadAll(rc)
			if err != nil {
				return err
			}

			// A fast wrong answer is not a measurement.
			if !bytes.Equal(got, payload) {
				return fmt.Errorf("downloaded %d bytes that do not match the %d uploaded", len(got), len(payload))
			}

			return nil
		})
		if err != nil {
			f.add(mDown, t.name, err)
		} else {
			s.add(mDown, t.name, d)
		}
	}

	d, err = timed(func() error {
		if err := sb.Pause(ctx); err != nil {
			return fmt.Errorf("pause: %w", err)
		}

		resumed, err := sb.Resume(ctx)
		if err != nil {
			return fmt.Errorf("resume: %w", err)
		}

		sb = resumed

		return command(ctx, sb, "true")
	})
	if err != nil {
		f.add(mResume, t.name, err)
	} else {
		s.add(mResume, t.name, d)
	}

	if cfg.idleWait > 0 {
		time.Sleep(cfg.idleWait)

		d, err = timed(func() error { return command(ctx, sb, "true") })
		if err != nil {
			f.add(mIdle, t.name, err)
		} else {
			s.add(mIdle, t.name, d)
		}
	}
}

func command(ctx context.Context, sb *opensandbox.Sandbox, cmd string) error {
	ex, err := sb.RunCommand(ctx, cmd, nil)
	if err != nil {
		return err
	}

	if ex.Error != nil {
		return fmt.Errorf("%q: %s: %s", cmd, ex.Error.Name, ex.Error.Value)
	}

	if ex.ExitCode != nil && *ex.ExitCode != 0 {
		return fmt.Errorf("%q exited %d", cmd, *ex.ExitCode)
	}

	return nil
}

func timed(fn func() error) (time.Duration, error) {
	start := time.Now()
	err := fn()

	return time.Since(start), err
}

func table(w io.Writer, names []string, s samples, f failures, cfg config, rounds int) bool {
	metrics := []string{mCreate, mRTT, mUp, mDown, mResume}
	if cfg.idleWait > 0 {
		metrics = append(metrics, mIdle)
	}

	fmt.Fprintf(w, "\n%d rounds, targets interleaved and rotated each round; image %s; %d round trips per sandbox; "+
		"%d-byte file.\n\n", rounds, cfg.image, cfg.rtt, cfg.size)
	fmt.Fprintln(w, "| metric | target | n | p50 ms | p90 ms | min ms | max ms | errors |")
	fmt.Fprintln(w, "|---|---|---:|---:|---:|---:|---:|---:|")

	measured := false

	for _, m := range metrics {
		for _, t := range names {
			xs := s[m][t]
			errs := f[m][t]

			if len(xs) == 0 {
				fmt.Fprintf(w, "| %s | %s | 0 | - | - | - | - | %d |\n", m, t, len(errs))
				continue
			}

			measured = true
			sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
			fmt.Fprintf(w, "| %s | %s | %d | %s | %s | %s | %s | %d |\n",
				m, t, len(xs), ms(pct(xs, 50)), ms(pct(xs, 90)), ms(xs[0]), ms(xs[len(xs)-1]), len(errs))
		}
	}

	fmt.Fprintln(w, "\nA difference between targets smaller than either target's min-max spread is not a result.")

	var errLines []string

	for _, m := range metrics {
		for _, t := range names {
			if errs := f[m][t]; len(errs) > 0 {
				errLines = append(errLines, fmt.Sprintf("- %s / %s: %s", m, t, errs[0]))
			}
		}
	}

	if len(errLines) > 0 {
		fmt.Fprintln(w, "\nFirst error per metric:")

		for _, l := range errLines {
			fmt.Fprintln(w, l)
		}
	}

	return measured
}

// pct is nearest-rank on sorted samples.
func pct(sorted []time.Duration, p int) time.Duration {
	i := (p*len(sorted)+99)/100 - 1
	if i < 0 {
		i = 0
	}

	return sorted[i]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond)) }
