//go:build unix

package execd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// defaultGrace is upstream's EXECD_API_GRACE_SHUTDOWN default.
const defaultGrace = 200 * time.Millisecond

// forwarded are the signals passed on to the child, the set upstream forwards. SIGCHLD is the
// reaper's and SIGKILL/SIGSTOP cannot be caught.
var forwarded = []os.Signal{
	syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT,
	syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH,
}

// Main is `sbx execd`. It returns the process exit status: the child's when there is one.
func Main(args []string) int {
	return run(args, os.Stderr)
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("execd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: sbx execd [--addr %s] [-- command args...]\n\n"+
			"The OpenSandbox execd API, for inside a sandbox. Environment:\n"+
			"  %s  token clients must send in %s (unset: no auth)\n"+
			"  %s  how long to keep serving after the command exits (default %s)\n"+
			"  %s  file of KEY=VALUE lines added to every command's environment\n",
			DefaultAddr, EnvAccessToken, AccessTokenHeader, EnvGraceShutdown, defaultGrace, EnvExtraEnvs)
	}

	addr := fs.String("addr", DefaultAddr, "address to serve the execd API on")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}

		return 2
	}

	logger := log.New(stderr, "sbx execd: ", log.LstdFlags)

	grace := defaultGrace

	if v := os.Getenv(EnvGraceShutdown); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			logger.Printf("%s=%q is not a duration; use a Go duration such as 200ms or 2s", EnvGraceShutdown, v)
			return 2
		}

		grace = d
	}

	// Read, then removed from our own environment, so no child - the entrypoint included -
	// inherits the token.
	token := os.Getenv(EnvAccessToken)
	_ = os.Unsetenv(EnvAccessToken)

	child := fs.Args()

	// Reap when orphans will come to us: as PID 1 always, and on Linux whenever there is an
	// entrypoint, by asking to be its subreaper.
	reap := os.Getpid() == 1
	if !reap && len(child) > 0 && becomeSubreaper() == nil {
		reap = true
	}

	srv, err := New(Options{AccessToken: token, Reap: reap, Logger: logger})
	if err != nil {
		logger.Print(err)
		return 1
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Printf("listen on %s: %v; pass a free address with --addr", *addr, err)
		return 1
	}

	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second, ErrorLog: logger}

	served := make(chan error, 1)

	go func() { served <- hs.Serve(ln) }()

	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, forwarded...)
	defer signal.Stop(sigs)

	if len(child) == 0 {
		return serveOnly(hs, served, sigs, logger)
	}

	cmd := exec.Command(child[0], child[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	p, err := srv.procs.start(cmd)
	if err != nil {
		logger.Printf("start %q: %v", child[0], err)
		shutdown(hs, 0)

		// 127 and 126 are what a shell would have exited with, so an orchestrator that reads
		// the container's exit code sees the usual meaning.
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return 127
		}

		return 126
	}

	for {
		select {
		case sig := <-sigs:
			// By pid, not through os.Process: finish() releases that handle concurrently.
			if s, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(p.pid, s)
			}
		case err := <-served:
			// The API died under a running entrypoint. Keep the entrypoint's lifecycle - the
			// sandbox's workload matters more than its control plane - but say so.
			logger.Printf("execd API stopped: %v; the command keeps running without it", err)

			served = nil
		case <-p.done:
			code := p.exitCode()

			// The grace keeps the API up for a moment, so a client that ran the command
			// that ended the entrypoint can still read how it ended.
			time.Sleep(grace)
			shutdown(hs, grace)

			return code
		}
	}
}

func serveOnly(hs *http.Server, served chan error, sigs chan os.Signal, logger *log.Logger) int {
	for {
		select {
		case err := <-served:
			logger.Printf("serve: %v", err)
			return 1
		case sig := <-sigs:
			if sig == syscall.SIGTERM || sig == syscall.SIGINT || sig == syscall.SIGQUIT {
				shutdown(hs, 5*time.Second)
				return 0
			}
		}
	}
}

// shutdown stops accepting and gives in-flight requests up to wait before cutting them.
// Streams never finish on their own, so the cut is expected, not an error.
func shutdown(hs *http.Server, wait time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	if err := hs.Shutdown(ctx); err != nil {
		_ = hs.Close()
	}
}
