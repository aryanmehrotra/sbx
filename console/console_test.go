package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The zero-dependency root module is a product claim - it is a README badge, it is why
// `go install` resolves nothing but stdlib, and it is the entire reason this console is a
// separate module. The plan said to prove it with a test rather than by looking, because an
// errant `go get` in the root would otherwise regress it silently.
func TestRootModuleHasNoDependencies(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "go.mod"))
	if err != nil {
		t.Fatalf("reading the root go.mod: %v", err)
	}

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "require") {
			t.Fatalf("the root module gained a dependency: %q\n"+
				"sbx ships one static binary with nothing outside the standard library. "+
				"Anything that needs a dependency belongs in a module like this one.", line)
		}
	}
}

// The console must never be pulled into the root build graph, or `go install sbx` starts
// resolving GoFr.
func TestConsoleIsNotInTheRootBuildGraph(t *testing.T) {
	out, err := exec.Command("go", "list", "./...").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}

	cmd := exec.Command("go", "list", "./...")
	cmd.Dir = ".."

	rootOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list in the root module: %v", err)
	}

	if strings.Contains(string(rootOut), "/console") {
		t.Fatalf("the root module builds the console:\n%s", rootOut)
	}

	_ = out
}

// -race, a writer streaming events and a reader scraping at the same time. The store is
// shared mutable state written by the ingest goroutine and read by an HTTP handler, which
// is the exact shape of bug the race detector exists to find.
func TestStoreUnderConcurrentIngestAndScrape(t *testing.T) {
	s := &store{svc: map[string]*service{}}

	var wg sync.WaitGroup

	for w := range 8 {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			for i := range 500 {
				s.Wake(fmt.Sprintf("sandbox-%d", w%3), "redis", int64(i))
				s.Sleep(fmt.Sprintf("sandbox-%d", w%3), "redis")
			}
		}(w)
	}

	// Readers, concurrent with every one of those writes.
	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 500 {
				for _, v := range s.Snapshot() {
					_ = v.Wakes
				}
			}
		}()
	}

	wg.Wait()

	if got := len(s.Snapshot()); got != 3 {
		t.Fatalf("services: got %d, want 3", got)
	}
}

// The scale row: 10^5 lines, with the numbers reported rather than asserted. The point is
// to know the cost, and to find out whether ingest or scraping is the binding limit.
func TestIngestScale(t *testing.T) {
	const lines = 100_000

	var b strings.Builder
	for i := range lines {
		fmt.Fprintf(&b, `{"sandbox":"br","service":"redis","event":"woke","durationMs":%d}`+"\n", i%2000)
	}

	s := &store{svc: map[string]*service{}}

	start := time.Now()

	if err := Ingest(strings.NewReader(b.String()), s); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	took := time.Since(start)

	if got := s.Snapshot()[0].Wakes; got != lines {
		t.Fatalf("wakes: got %d, want %d", got, lines)
	}

	t.Logf("ingested %d lines in %s - %.0f lines/sec, %.1f µs/line",
		lines, took.Round(time.Millisecond),
		float64(lines)/took.Seconds(), float64(took.Microseconds())/lines)
}

// A single oversized line must cost one sample, not every sample for the rest of the
// process's life. This is the failure a Scanner would have turned into a permanent halt.
func TestOversizedLineDoesNotStopTheStream(t *testing.T) {
	huge := `{"sandbox":"br","service":"redis","event":"woke","durationMs":1,"pad":"` +
		strings.Repeat("x", 300_000) + `"}`

	in := `{"sandbox":"br","service":"redis","event":"woke","durationMs":10}` + "\n" +
		huge + "\n" +
		`{"sandbox":"br","service":"redis","event":"woke","durationMs":20}` + "\n"

	var r recorder
	if err := Ingest(strings.NewReader(in), &r); err != nil {
		t.Fatalf("Ingest returned an error instead of carrying on: %v", err)
	}

	// The oversized line may or may not decode; what must not happen is losing the line
	// after it.
	if len(r.wakes) < 2 {
		t.Fatalf("wakes: got %v, want the lines either side of the oversized one", r.wakes)
	}

	if r.wakes[len(r.wakes)-1] != 20 {
		t.Errorf("the line after the oversized one was lost: %v", r.wakes)
	}
}

// The daemon exiting is not an error condition for the console: EOF means the thing it was
// watching stopped, and the console must keep serving what it already knows.
func TestDaemonExitingMidStreamIsNotAnError(t *testing.T) {
	var r recorder

	in := `{"sandbox":"br","service":"redis","event":"woke","durationMs":1}` + "\n" +
		`{"sandbox":"br","service":"redis","event":"woke","durationMs":2}` // no trailing newline

	if err := Ingest(strings.NewReader(in), &r); err != nil {
		t.Fatalf("EOF reported as an error: %v", err)
	}

	if len(r.wakes) != 2 {
		t.Fatalf("wakes: got %v, want both - the last line had no newline", r.wakes)
	}
}

// Everything below runs the real binary. It is the only way to assert on ports, on the
// served payload, and on several consoles coexisting.
func buildConsole(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "console")

	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Skipf("cannot build the console here: %v\n%s", err, out)
	}

	return bin
}

type running struct {
	cmd  *exec.Cmd
	disc discovery
}

func start(t *testing.T, bin, state string) *running {
	t.Helper()

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "SBX_STATE="+state)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// Wait for the discovery file this process wrote, not for a line of text.
	path := filepath.Join(state, fmt.Sprintf("console-%d.json", cmd.Process.Pid))

	var d discovery

	for range 100 {
		if body, err := os.ReadFile(path); err == nil && json.Unmarshal(body, &d) == nil && d.HTTP != 0 {
			go io.Copy(io.Discard, stdout)

			return &running{cmd: cmd, disc: d}
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("console never recorded its ports at %s", path)

	return nil
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()

	c := &http.Client{Timeout: 5 * time.Second}

	resp, err := c.Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(body)
}

// The criterion the fixed-default design fails outright: GoFr refuses to start on a taken
// port, and sbx runs one daemon per branch, per worktree, per CI job.
func TestThreeConsolesRunConcurrently(t *testing.T) {
	bin := buildConsole(t)
	state := t.TempDir()

	seen := map[int]bool{}

	for i := range 3 {
		r := start(t, bin, state)

		if seen[r.disc.HTTP] || seen[r.disc.Metrics] {
			t.Fatalf("console %d reused a port: %+v", i, r.disc)
		}

		seen[r.disc.HTTP] = true
		seen[r.disc.Metrics] = true

		if code, _ := get(t, r.disc.Health); code != http.StatusOK {
			t.Errorf("console %d health: got %d, want 200", i, code)
		}

		if code, _ := get(t, r.disc.Scrape); code != http.StatusOK {
			t.Errorf("console %d metrics: got %d, want 200", i, code)
		}
	}
}

// /metrics and /api/sandboxes are unauthenticated by design, so what they serve is what
// anyone on the host can read. This asserts on the real bytes rather than on intent.
func TestServedPayloadLeaksNothing(t *testing.T) {
	bin := buildConsole(t)
	state := t.TempDir()

	// A secret that would be in this process's environment if anything echoed env back.
	t.Setenv("SBX_TEST_SECRET", "hunter2-do-not-serve")

	r := start(t, bin, state)

	for _, u := range []string{r.disc.Scrape, fmt.Sprintf("http://127.0.0.1:%d/api/sandboxes", r.disc.HTTP)} {
		code, body := get(t, u)
		if code != http.StatusOK {
			t.Fatalf("%s: got %d", u, code)
		}

		for _, forbidden := range []string{"hunter2", "SBX_TEST_SECRET", "POSTGRES_PASSWORD", "AWS_", "sandbox.json"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s served %q:\n%s", u, forbidden, body)
			}
		}
	}
}
