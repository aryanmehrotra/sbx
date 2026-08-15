// sbx gives every branch, task or agent its own copy of a project's backing services,
// and charges nothing for the ones nobody is using.
//
// A repo declares its services once, in sandbox.json. Anyone — a person, an agent, CI —
// creates a sandbox from that spec and gets their own containers on their own ports, with
// their own data. Those containers are stopped by default. `sbx serve` owns their public
// ports: a connection wakes the container behind it and is spliced through, and a few idle
// minutes stops it again. The caller sees a slow first query, never a failure.
//
//	sbx create feature-x          # realise the spec, once
//	sbx env feature-x             # exports your existing tooling already reads
//	sbx ready feature-x           # wake it and block until it is serving
//	sbx add feature-x pg --image postgres:16 --port 5432
//	sbx list
//	sbx rm feature-x
//	sbx serve --idle 5m           # the daemon; run one per machine
//
// There is deliberately no start and no stop. Whatever can start a sandbox eventually
// becomes the thing that left one running, and then two components believe they own the
// lifecycle. Only the daemon does, because only the daemon can see demand.
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aryanmehrotra/sbx/internal/cli"
	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/tunnel"
)

const defaultSpec = "sandbox.json"

// specFrom resolves --template or --spec into a Spec.
//
// --template exists so that the first thing somebody does with this is not "author a JSON
// file". An agent told to spin up a Postgres should be able to, in one line, with nothing on
// disk.
func specPath(templateName, path string) (string, error) {
	if templateName == "" {
		return path, nil
	}

	return MaterializeTemplate(templateName)
}

// version is stamped by scripts/release.sh. "dev" means somebody built it themselves,
// which is worth knowing when a bug report says the wake behaved oddly.
var version = "dev"

// Every command that touches a backend takes these, so a sandbox can be created on this
// laptop and the identical spec realised in a cluster without editing anything.
func backendFlags(fs *flag.FlagSet) (kind, socket, namespace, isolation *string) {
	kind = fs.String("provider", cmp.Or(os.Getenv("SBX_PROVIDER_KIND"), "docker"), "docker | kubernetes")
	socket = fs.String("socket", "", "docker endpoint; defaults to DOCKER_HOST, then the active docker context")
	namespace = fs.String("namespace", cmp.Or(os.Getenv("SBX_NAMESPACE"), "sbx"), "kubernetes namespace")
	isolation = fs.String("isolation", cmp.Or(os.Getenv("SBX_ISOLATION"), string(provider.IsolationContainer)),
		"container | gvisor | kata")

	return kind, socket, namespace, isolation
}

// resolve turns the backend flags into a provider and a validated isolation tier.
func resolve(kind, socket, namespace, isolation string) (provider.Provider, provider.Isolation, error) {
	iso := provider.Isolation(isolation)
	if !iso.Valid() {
		return nil, "", fmt.Errorf("unknown isolation %q (want container, gvisor or kata)", isolation)
	}

	p, err := provider.For(kind, socket, namespace)

	return p, iso, err
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	logs.Version = version

	if err := dispatch(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "sbx: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(cmd string, args []string) error {
	switch cmd {
	case "create":
		fs := flag.NewFlagSet("create", flag.ExitOnError)
		spec := fs.String("spec", defaultSpec, "path to sandbox.json")
		tmpl := fs.String("template", "", "use a built-in spec instead: "+strings.Join(TemplateNames(), ", "))
		optional := fs.Bool("optional", false, "include services marked optional")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 1)
		_ = fs.Parse(rest)

		if len(positional) < 1 {
			return fmt.Errorf("missing sandbox name")
		}

		p, iso, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		path, err := specPath(*tmpl, *spec)
		if err != nil {
			return err
		}

		return cli.Create(context.Background(), p, path, positional[0], *optional, iso)

	case "env":
		fs := flag.NewFlagSet("env", flag.ExitOnError)
		spec := fs.String("spec", defaultSpec, "path to sandbox.json")
		tmpl := fs.String("template", "", "use a built-in spec instead")
		shell := fs.String("shell", "", "posix | fish | powershell | cmd | json; detected if unset")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 1)
		_ = fs.Parse(rest)

		if len(positional) < 1 {
			return fmt.Errorf("missing sandbox name")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		path, err := specPath(*tmpl, *spec)
		if err != nil {
			return err
		}

		return cli.Env(context.Background(), p, path, positional[0], *shell)

	case "ready":
		fs := flag.NewFlagSet("ready", flag.ExitOnError)
		timeout := fs.Duration("timeout", 90*time.Second, "give up after this long")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 1)
		_ = fs.Parse(rest)

		if len(positional) < 1 {
			return fmt.Errorf("missing sandbox name")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return cli.Ready(context.Background(), p, positional[0], *timeout)

	case "add":
		return runAdd(args)

	case "snapshot":
		fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 2)
		_ = fs.Parse(rest)

		if len(positional) < 2 {
			return fmt.Errorf("usage: sbx snapshot <sandbox> <name>")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		_, err = cli.Snapshot(context.Background(), p, positional[0], positional[1])

		return err

	case "fork":
		fs := flag.NewFlagSet("fork", flag.ExitOnError)
		spec := fs.String("spec", defaultSpec, "path to the spec the snapshot came from")
		tmpl := fs.String("template", "", "use a built-in spec instead: "+strings.Join(TemplateNames(), ", "))
		optional := fs.Bool("optional", false, "include services marked optional")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 2)
		_ = fs.Parse(rest)

		if len(positional) < 2 {
			return fmt.Errorf("usage: sbx fork <snapshot> <new-sandbox>")
		}

		p, iso, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		path, err := specPath(*tmpl, *spec)
		if err != nil {
			return err
		}

		return cli.Fork(context.Background(), p, path, positional[0], positional[1], *optional, iso)

	case "doctor":
		fs := flag.NewFlagSet("doctor", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "machine-readable")
		_ = fs.Parse(args)

		return cli.PrintReport(os.Stdout, cli.Doctor(context.Background()), *asJSON)

	case "exec":
		fs := flag.NewFlagSet("exec", flag.ExitOnError)
		kind, socket, ns, isolation := backendFlags(fs)
		tty := fs.Bool("t", false, "attach a terminal — for a shell, psql, redis-cli")

		// Parse first, then take positionals from what is left. flag stops at the first
		// non-flag argument, so `sbx exec -t br pg psql -U app` gives sbx the -t and hands
		// psql its own -U untouched — which is how docker and kubectl behave, and what
		// anyone typing this expects. Splitting positionals first, as the other commands
		// do, made a LEADING flag consume the sandbox name and print a usage error.
		_ = fs.Parse(args)

		positional := fs.Args()
		if len(positional) < 3 {
			return fmt.Errorf("usage: sbx exec [-t] <sandbox> <service> <command>...")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return cli.Exec(context.Background(), p, positional[0], positional[1], positional[2:], *tty)

	case "logs":
		fs := flag.NewFlagSet("logs", flag.ExitOnError)
		lines := fs.Int("tail", 100, "how many lines")
		follow := fs.Bool("f", false, "keep streaming")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 2)
		_ = fs.Parse(rest)

		if len(positional) < 1 {
			return fmt.Errorf("usage: sbx logs <sandbox> [service] [--tail N] [-f]")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		svc := ""
		if len(positional) > 1 {
			svc = positional[1]
		}

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		return cli.Logs(ctx, p, positional[0], svc, *lines, *follow)

	case "cp":
		fs := flag.NewFlagSet("cp", flag.ExitOnError)
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 4)
		_ = fs.Parse(rest)

		if len(positional) < 4 {
			return fmt.Errorf("usage: sbx cp <sandbox> <service> <src> <dst>   (prefix the inside path with :)")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return cli.Copy(context.Background(), p, positional[0], positional[1], positional[2], positional[3])

	case "url":
		fs := flag.NewFlagSet("url", flag.ExitOnError)
		via := fs.String("via", "", "cloudflared | ngrok | ssh; detected if unset")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 2)
		_ = fs.Parse(rest)

		if len(positional) < 2 {
			return fmt.Errorf("usage: sbx url <sandbox> <service> [--via cloudflared|ngrok|ssh]")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		// main resolves the sandbox to a port; the tunnel package never learns what a
		// sandbox is, which is why it stays readable on its own.
		port, err := cli.WakePort(ctx, p, positional[0], positional[1])
		if err != nil {
			return err
		}

		return tunnel.Open(ctx, positional[0]+"/"+positional[1], port, *via)

	case "templates":
		for _, t := range TemplateNames() {
			fmt.Println(t)
		}

		return nil

	case "list":
		fs := flag.NewFlagSet("list", flag.ExitOnError)
		kind, socket, ns, isolation := backendFlags(fs)
		_ = fs.Parse(args)

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return cli.List(context.Background(), p)

	case "rm":
		fs := flag.NewFlagSet("rm", flag.ExitOnError)
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 1)
		_ = fs.Parse(rest)

		if len(positional) < 1 {
			return fmt.Errorf("missing sandbox name")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return cli.Remove(context.Background(), p, positional[0])

	case "selftest":
		fs := flag.NewFlagSet("selftest", flag.ExitOnError)
		keep := fs.Bool("keep", false, "leave the sandbox behind for inspection")
		kind, socket, ns, isolation := backendFlags(fs)
		_ = fs.Parse(args)

		p, iso, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return cli.Selftest(context.Background(), p, iso, *keep)

	case "serve":
		return daemon.Serve(args)

	case "version", "--version", "-v":
		fmt.Println(version)
		return nil

	case "-h", "--help", "help":
		usage()
		return nil

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// runAdd is the agent-facing path: put a service nobody declared into an existing sandbox.
func runAdd(args []string) error {
	// `sbx add <sandbox> <service> --image ...` reads the way a person would write it, but
	// Go's flag package stops at the first non-flag argument, so the flags after the two
	// names would be silently ignored — and the command would fail claiming --image was
	// missing while it sat right there on the line. Split the leading names off first.
	positional, rest := splitPositional(args, 2)

	fs := flag.NewFlagSet("add", flag.ExitOnError)
	image := fs.String("image", "", "container image (required)")
	ports := fs.String("port", "", "comma-separated container ports (required)")
	health := fs.String("health", "", "command run inside the container to test readiness")
	volume := fs.String("volume", "", "container path to persist")
	envs := fs.String("env", "", "comma-separated KEY=VALUE pairs")
	spec := fs.String("spec", defaultSpec, "path to sandbox.json, whose reservations are respected")
	kind, socket, ns, isolation := backendFlags(fs)
	_ = fs.Parse(rest)

	if len(positional) < 2 {
		return fmt.Errorf("usage: sbx add <sandbox> <service> --image IMG --port PORT")
	}

	sandbox, service := positional[0], positional[1]

	if *image == "" || *ports == "" {
		return fmt.Errorf("--image and --port are required")
	}

	var cps []int

	for _, p := range strings.Split(*ports, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return fmt.Errorf("bad port %q: %w", p, err)
		}

		cps = append(cps, n)
	}

	env := map[string]string{}

	if *envs != "" {
		for _, kv := range strings.Split(*envs, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("bad --env entry %q, want KEY=VALUE", kv)
			}

			env[strings.TrimSpace(k)] = v
		}
	}

	// Anything left over is passed to the image as its command, so an agent can add a
	// service that needs arguments without sbx having to know what they mean.
	extra := fs.Args()

	if *health == "" {
		fmt.Fprintln(os.Stderr,
			"sbx: warning: no --health given, so waking this service can only wait for its\n"+
				"     published port, which docker answers before the server does. The first\n"+
				"     query after a wake may hit a socket that is about to close.")
	}

	p, iso, err := resolve(*kind, *socket, *ns, *isolation)
	if err != nil {
		return err
	}

	return cli.Add(context.Background(), p, *spec, sandbox, service, *image, cps, *health, env, *volume, extra, iso)
}

// splitPositional peels up to n leading non-flag arguments off the front.
func splitPositional(args []string, n int) (positional, rest []string) {
	i := 0
	for i < len(args) && i < n && !strings.HasPrefix(args[i], "-") {
		i++
	}

	return args[:i], args[i:]
}

func usage() {
	fmt.Fprint(os.Stderr, `sbx — per-branch sandboxes that sleep when nobody is using them

  sbx create <sandbox> [--spec sandbox.json | --template NAME] [--optional]
  sbx env    <sandbox> [--spec sandbox.json] [--shell posix|fish|powershell|cmd|json]
  sbx ready  <sandbox> [--timeout 90s]
  sbx add    <sandbox> <service> --image IMG --port 5432[,...] [--health CMD]
                                 [--env K=V,...] [--volume PATH] [ARGS...]
  sbx snapshot <sandbox> <name>                 save every service's filesystem
  sbx fork   <snapshot> <new-sandbox> [--spec]  a new sandbox from that state
  sbx doctor [--json]                           what this machine can and cannot do
  sbx exec   [-t] <sandbox> <service> <command>...   -t attaches a terminal
  sbx logs   <sandbox> [service] [--tail N] [-f]   all services if none named
  sbx cp     <sandbox> <service> <src> <dst>    (inside path is prefixed with :)
  sbx url    <sandbox> <service> [--via cloudflared|ngrok|ssh]
  sbx list
  sbx rm     <sandbox>
  sbx serve  [--idle 5m] [--socket PATH]
  sbx selftest [--provider ...] [--keep]     prove it works here, in about a minute

Templates (no spec file needed): sbx templates

Backend (any command): --provider docker|kubernetes  --namespace NS
                       --isolation container|gvisor|kata

There is no start and no stop: connecting to a sandbox port wakes it, and idleness
sleeps it. Run one "sbx serve" per machine.
`)
}
