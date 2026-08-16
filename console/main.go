// Command console serves metrics, health and a read-only API for a running sbx daemon.
//
//	sbx serve 2>&1 | console
//
// It is a separate module on purpose. The root module has no dependencies and that is a
// product claim, not an accident: `go install github.com/aryanmehrotra/sbx@latest` resolves
// nothing but the standard library, and the daemon is small because nothing was linked into
// it that did not have to be.
//
// The daemon does not know this exists. It already writes structured JSON for every wake and
// sleep; this reads that stream. There is no callback, no shared state file and no socket
// between them, so there is no mechanism by which observability could slow the splice down.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"gofr.dev/pkg/gofr"
)

// freePort asks the kernel for a port nobody is using, then lets it go.
//
// There is a window between closing and GoFr binding, and it is accepted deliberately: the
// alternative is passing a listening fd into a framework that wants to open its own, which
// buys a guarantee this does not need. What it must not do is use GoFr's defaults - GoFr
// refuses to start when a port is taken, and sbx is multi-instance by design (one daemon per
// branch, per worktree, per CI job), so fixed ports mean the second console dies on boot.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port, nil
}

// ports fixes all three of GoFr's listeners to ports we know are free. GRPC_PORT is set even
// though no service is registered: GoFr only binds it on registration, and leaving it at the
// default would arm a collision for whoever adds the first service.
func ports() (http, metrics int, err error) {
	if http, err = freePort(); err != nil {
		return 0, 0, fmt.Errorf("no free http port: %w", err)
	}

	if metrics, err = freePort(); err != nil {
		return 0, 0, fmt.Errorf("no free metrics port: %w", err)
	}

	grpc, err := freePort()
	if err != nil {
		return 0, 0, fmt.Errorf("no free grpc port: %w", err)
	}

	// GoFr reports the number of active servers home unless told not to. A tool whose job
	// is to watch your infrastructure should not be the thing making an outbound call you
	// did not ask for, and it must also work on a machine with no network at all.
	os.Setenv("GOFR_TELEMETRY", "false")

	os.Setenv("HTTP_PORT", fmt.Sprint(http))
	os.Setenv("METRICS_PORT", fmt.Sprint(metrics))
	os.Setenv("GRPC_PORT", fmt.Sprint(grpc))

	return http, metrics, nil
}

// sandboxes reports what the daemon is fronting. Read-only by construction: the console
// has no verb that changes a sandbox, because two components owning one lifecycle is the
// mistake ARCHITECTURE.md opens by warning about.
func sandboxes(ctx *gofr.Context) (any, error) {
	return map[string]any{"sandboxes": state.Snapshot()}, nil
}

// discovery is what a scrape config reads to find a console that chose its own ports.
type discovery struct {
	HTTP    int    `json:"http"`
	Metrics int    `json:"metrics"`
	PID     int    `json:"pid"`
	Health  string `json:"health"`
	Scrape  string `json:"scrape"`
}

// StateDir is where the file lands: $SBX_STATE if set, else the user's cache dir, else the
// working directory. Per-PID, because several consoles run at once by design and one file
// per machine would mean the last one started hid all the others.
func StateDir() string {
	if d := os.Getenv("SBX_STATE"); d != "" {
		return d
	}

	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "sbx")
	}

	return "."
}

func writeDiscovery(http, metrics int) (string, error) {
	dir := StateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	path := filepath.Join(dir, fmt.Sprintf("console-%d.json", os.Getpid()))

	body, err := json.Marshal(discovery{
		HTTP:    http,
		Metrics: metrics,
		PID:     os.Getpid(),
		Health:  fmt.Sprintf("http://127.0.0.1:%d/.well-known/health", http),
		Scrape:  fmt.Sprintf("http://127.0.0.1:%d/metrics", metrics),
	})
	if err != nil {
		return "", err
	}

	return path, os.WriteFile(path, append(body, '\n'), 0o644)
}

func main() {
	http, metrics, err := ports()
	if err != nil {
		fmt.Fprintln(os.Stderr, "console:", err)
		os.Exit(1)
	}

	// Printed before Run, because Run does not return.
	fmt.Printf("console  http :%d  metrics :%d/metrics  health :%d/.well-known/health\n",
		http, metrics, http)

	// And written down, because grepping a log is not an interface a scrape config can use.
	// Discovered ports are only useful if something other than a human can find them.
	if path, err := writeDiscovery(http, metrics); err != nil {
		fmt.Fprintln(os.Stderr, "console: could not record ports:", err)
	} else {
		fmt.Println("console  ports recorded in", path)
	}

	app := gofr.New()

	// GoFr binds the HTTP listener only when a route exists, so health and /alive are
	// unreachable on a routeless app - the spike found this by getting 000 from both.
	app.GET("/api/sandboxes", sandboxes)

	m := app.Metrics()
	m.NewCounter("sbx_wakes_total", "wakes served, by sandbox and service")
	m.NewCounter("sbx_sleeps_total", "sleeps, by sandbox and service")
	m.NewCounter("sbx_wake_failures_total", "wakes that did not happen")
	// Buckets in milliseconds, spanning a docker wake (~200ms) to a cluster one (~1.5s)
	// and past it, because the interesting question is always the tail.
	m.NewHistogram("sbx_wake_duration_ms", "how long a caller waited for a wake",
		50, 100, 200, 400, 800, 1600, 3200, 6400)

	// Reading the daemon's stream is the whole job, and it must not block Run().
	go func() {
		if err := Ingest(os.Stdin, &sink{m: m}); err != nil {
			fmt.Fprintln(os.Stderr, "console: ingest stopped:", err)
		}
	}()

	app.Run()
}
