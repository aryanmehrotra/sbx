package app

// STUB - delete this file when internal/execd lands.
//
// The real `sbx execd` is being written in parallel (internal/execd, owned by another branch).
// The lifecycle API needs *something* at /opt/sbx/sbx execd to test against: a process that
// answers /ping, runs the user's entrypoint as its child, and exits with its code. That is all
// this does. Every other execd endpoint answers 501, so nothing can mistake it for the real one.
//
// It registers itself through inContainer, so deleting this file removes the command and
// nothing else needs editing.

import (
	"errors"
	"flag"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

func init() { inContainer["execd"] = execdStub }

func execdStub(args []string) int {
	fs := flag.NewFlagSet("execd", flag.ContinueOnError)
	addr := fs.String("addr", ":44772", "listen address")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"code":"NOT_IMPLEMENTED","message":"this is sbx's stub execd; the real one is internal/execd"}`))
	})

	srv := &http.Server{Addr: *addr, Handler: mux}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			os.Stderr.WriteString("execd: " + err.Error() + "\n")
			os.Exit(1)
		}
	}()

	child := fs.Args()
	if len(child) == 0 {
		select {}
	}

	cmd := exec.Command(child[0], child[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	// The token is execd's credential, not the workload's.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "EXECD_ACCESS_TOKEN=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}

	if err := cmd.Start(); err != nil {
		os.Stderr.WriteString("execd: " + err.Error() + "\n")
		return 127
	}

	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)

	go func() {
		for s := range sig {
			_ = cmd.Process.Signal(s)
		}
	}()

	err := cmd.Wait()

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}

	if err != nil {
		return 1
	}

	return 0
}
