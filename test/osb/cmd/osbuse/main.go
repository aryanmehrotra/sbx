// Command osbuse is the Go half of scripts/osb-usecases-e2e.sh: the flows a user or an agent
// actually performs against `sbx serve --osb-addr`, driven through OpenSandbox's own Go SDK.
//
// The conformance suite (scripts/osb-conformance.sh) proves the API answers the way upstream's
// tests expect, one call at a time. It does not prove that a sequence someone would really run
// - write a file, snapshot, fork, read it back; freeze, wake, find the process still counting -
// comes out right. Each subcommand here is one such sequence, asserting on what a caller can
// observe: output, file contents, exit codes, a container's start time, a refused connection.
//
// The shell owns processes (the daemon, its flags, restarts, the trap); this owns everything
// that speaks HTTP or parses JSON. Every subcommand removes what it created, prints one line per
// assertion, and exits 1 if any failed.
//
//	osbuse <case> -url http://127.0.0.1:P -key K [case flags]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

// T collects the outcome of one case. A failed assertion is recorded and the case carries on
// where it can, so one run shows every broken step rather than the first.
type T struct {
	failed int
}

func (t *T) ok(format string, a ...any) { fmt.Printf("    ok   %s\n", fmt.Sprintf(format, a...)) }

func (t *T) fail(format string, a ...any) {
	t.failed++
	fmt.Printf("    FAIL %s\n", fmt.Sprintf(format, a...))
}

func (t *T) check(cond bool, format string, a ...any) bool {
	if cond {
		t.ok(format, a...)
	} else {
		t.fail(format, a...)
	}

	return cond
}

// fatal is for a step later steps cannot run without; the case stops there.
type fatal struct{ error }

func (t *T) must(err error, what string) {
	if err != nil {
		t.fail("%s: %v", what, err)
		panic(fatal{err})
	}
}

type env struct {
	cfg   opensandbox.ConnectionConfig
	url   string
	key   string
	args  map[string]*string
	image string
}

type caseFunc func(ctx context.Context, t *T, e *env)

var cases = map[string]struct {
	run   caseFunc
	flags []string // extra string flags this case reads
}{
	"mcp":         {caseMCP, []string{"sbx"}},
	"coding":      {caseCoding, nil},
	"interpreter": {caseInterpreter, nil},
	"idle":        {caseIdle, []string{"idle"}},
	"pause":       {casePause, nil},
	"expiry-make": {caseExpiryMake, []string{"out"}},
	"expiry-gone": {caseExpiryGone, []string{"id", "within"}},
	"egress":      {caseEgress, []string{"out"}},
	"fork":        {caseFork, nil},
	"volumes":     {caseVolumes, []string{"hostdir", "outside"}},
	"pty":         {casePTY, nil},
	"proxy":       {caseProxy, nil},
	"create":      {caseCreate, []string{"out", "extensions"}},
	"poke":        {casePoke, []string{"id"}},
	"kill":        {caseKill, []string{"id"}},
	"concurrent":  {caseConcurrent, []string{"n"}},
	"pool":        {casePool, []string{"n", "history"}},
}

func main() {
	if len(os.Args) < 2 || cases[os.Args[1]].run == nil {
		names := make([]string, 0, len(cases))
		for n := range cases {
			names = append(names, n)
		}

		fmt.Fprintf(os.Stderr, "usage: osbuse <%s> -url URL -key KEY\n", strings.Join(names, "|"))
		os.Exit(2)
	}

	name := os.Args[1]
	c := cases[name]

	fs := flag.NewFlagSet(name, flag.ExitOnError)
	url := fs.String("url", "", "lifecycle API base, http://host:port")
	key := fs.String("key", "", "OPEN-SANDBOX-API-KEY")
	image := fs.String("image", "python:3.11-slim", "image for cases that do not need a particular one")
	timeout := fs.Duration("timeout", 5*time.Minute, "the whole case")

	e := &env{args: map[string]*string{}}
	for _, f := range c.flags {
		e.args[f] = fs.String(f, "", f)
	}

	_ = fs.Parse(os.Args[2:])

	proto, host, ok := strings.Cut(*url, "://")
	if !ok || host == "" {
		fmt.Fprintln(os.Stderr, "osbuse: -url wants http://host:port")
		os.Exit(2)
	}

	e.url, e.key, e.image = strings.TrimSuffix(*url, "/"), *key, *image
	e.cfg = opensandbox.ConnectionConfig{
		Domain: strings.TrimSuffix(host, "/"), Protocol: proto, APIKey: *key,
		RequestTimeout: 3 * time.Minute,
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	t := &T{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, isFatal := r.(fatal); !isFatal {
					t.fail("panic: %v", r)
				}
			}
		}()
		c.run(ctx, t, e)
	}()

	if t.failed > 0 {
		os.Exit(1)
	}
}

func (e *env) arg(name string) string { return *e.args[name] }

// create makes a sandbox and registers its removal. Kill uses a fresh context: the case's own
// may be the thing that ran out.
func (e *env) create(ctx context.Context, t *T, opts opensandbox.SandboxCreateOptions) *opensandbox.Sandbox {
	// A fork names its snapshot INSTEAD of an image; the SDK refuses both.
	if opts.Image == "" && opts.SnapshotID == "" {
		opts.Image = e.image
	}

	if opts.ReadyTimeout == 0 {
		opts.ReadyTimeout = 2 * time.Minute
	}

	sb, err := opensandbox.CreateSandbox(ctx, e.cfg, opts)
	t.must(err, "create "+opts.Image)

	return sb
}

func kill(sb *opensandbox.Sandbox) {
	if sb == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_ = sb.Kill(ctx)
}

// run executes a foreground command and returns its stdout, stderr and exit code.
func run(ctx context.Context, sb *opensandbox.Sandbox, cmd string) (string, string, int, error) {
	ex, err := sb.RunCommand(ctx, cmd, nil)
	if err != nil {
		return "", "", -1, err
	}

	var out, errOut strings.Builder
	// Joined with newlines: execd streams a line per event, without its terminator.
	for _, m := range ex.Stdout {
		out.WriteString(m.Text + "\n")
	}

	for _, m := range ex.Stderr {
		errOut.WriteString(m.Text + "\n")
	}

	code := 0
	if ex.ExitCode != nil {
		code = *ex.ExitCode
	}

	if ex.Error != nil && code == 0 {
		code = -1
	}

	return out.String(), errOut.String(), code, nil
}

// execd is a raw execd client for the calls the high-level Sandbox does not wrap (status,
// logs, interrupt, PTY) - built exactly the way upstream's own e2e helper builds it.
func execd(ctx context.Context, sb *opensandbox.Sandbox) (*opensandbox.ExecdClient, string, string, error) {
	ep, err := sb.GetEndpoint(ctx, opensandbox.DefaultExecdPort)
	if err != nil {
		return nil, "", "", err
	}

	base := ep.Endpoint
	if !strings.HasPrefix(base, "http") {
		base = "http://" + base
	}

	token := ep.Headers["X-EXECD-ACCESS-TOKEN"]

	return opensandbox.NewExecdClient(base, token), base, token, nil
}

// container is the docker container behind an API sandbox (one service, named "sandbox").
func container(id string) string { return "sbx-" + id + "-sandbox" }

// inspect asks docker, through whatever DOCKER_HOST the shell set, for one field.
func inspect(name, format string) (string, error) {
	out, err := exec.Command("docker", "inspect", "-f", format, name).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}

	return strings.TrimSpace(string(out)), nil
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(ctx context.Context, within time.Duration, every time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return true
		}

		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}

		time.Sleep(every)
	}
}

func writeOut(path, s string) error {
	if path == "" {
		return errors.New("-out is required")
	}

	return os.WriteFile(path, []byte(s), 0o600)
}

// docker runs a docker command through the shell's DOCKER_HOST, for cleanup.
func docker(args ...string) error {
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return nil
}

// statusOf is the HTTP status behind an SDK error, or 0 when there was no response. The SDK's
// error text carries the server's code and message but not the status.
func statusOf(err error) int {
	var apiErr *opensandbox.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}

	return 0
}
