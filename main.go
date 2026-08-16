// sbx gives every branch, task or agent its own copy of a project's backing services,
// and charges nothing for the ones nobody is using.
//
// A repo declares its services once, in sandbox.json. Anyone - a person, an agent, CI -
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
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aryanmehrotra/sbx/internal/cli"
	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
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

// specFor resolves which spec a command should read, preferring what the user asked for and
// falling back to what the sandbox was created from.
//
// The fallback is why `--template postgres` no longer has to be repeated on every command
// for the same sandbox. It is only ever a default: an explicit --template or --spec wins, and
// if nothing was recorded this behaves exactly as it always did.
func specFor(fs *flag.FlagSet, sandbox, templateName, path string) (string, error) {
	if templateName != "" {
		return MaterializeTemplate(templateName)
	}

	// Whether --spec was *set*, not whether it differs from the default. Someone who writes
	// `--spec sandbox.json` for clarity has asked explicitly, and comparing the value would
	// silently redirect them to whatever was remembered - breaking the one guarantee this
	// feature makes.
	if wasSet(fs, "spec") {
		return path, nil
	}

	if o, ok := cli.Recall(sandbox); ok {
		if o.Template != "" {
			return MaterializeTemplate(o.Template)
		}

		return o.Spec, nil
	}

	return path, nil
}

// wasSet reports whether a flag was given on the command line at all.
func wasSet(fs *flag.FlagSet, name string) bool {
	found := false

	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})

	return found
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
		usage(os.Stderr)
		os.Exit(2)
	}

	logs.Version = version

	// Everything the daemon says about a sandbox goes into the journal as well as to its
	// stdout. Subscribing here rather than editing the wake path keeps file IO out of the
	// one code path this project publishes a number for.
	logs.Default.Observe(func(e logs.Entry) {
		if e.Event == "" {
			return // an ordinary line, not something that happened to a sandbox
		}

		history.Append(history.Record{
			Time: e.Time, Kind: "event",
			Sandbox: e.Sandbox, Service: e.Service,
			Event: e.Event, DurationMs: e.DurationMs, Message: e.Message,
			Failed: e.Level == "ERROR",
		})
	})

	// The daemon is recorded when it starts, not when it stops. Recording it on the way out
	// like everything else means a daemon that is still running - the normal state, and the
	// one worth knowing about - never appears in the journal at all.
	if os.Args[1] == "serve" {
		record(os.Args[1], os.Args[1:], nil)
	}

	err := dispatch(os.Args[1], os.Args[2:])

	if os.Args[1] != "serve" {
		record(os.Args[1], os.Args[1:], err)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx: %v\n", err)
		os.Exit(1)
	}
}

// record writes the invocation to the journal, so `sbx history` can answer "who did this to
// my sandbox" afterwards.
//
// Reading commands are skipped. A journal where every `sbx list` is a line is a journal
// nobody reads, and the question people actually ask is what *changed*.
func record(cmd string, argv []string, err error) {
	switch cmd {
	case "list", "env", "history", "doctor", "templates", "validate", "ready", "logs",
		"version", "--version", "-v", "help", "--help", "-h", "ui":
		return
	}

	r := history.Record{
		Kind:    "command",
		Sandbox: sandboxOf(cmd, argv),
		Command: history.Redact(append([]string{"sbx"}, argv...)),
	}

	if dir, e := os.Getwd(); e == nil {
		r.Dir = dir
	}

	if err != nil {
		r.Failed = true
		r.Error = err.Error()
	}

	history.Append(r)
}

// sandboxOf picks the sandbox name out of an argv, so history can be filtered by it. Every
// command that changes something takes it first, except serve, which is about the machine.
func sandboxOf(cmd string, argv []string) string {
	if cmd == "serve" || cmd == "selftest" || cmd == "prewarm" || cmd == "gc" {
		return ""
	}

	for _, a := range argv[1:] {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}

	return ""
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

		if err := cli.Create(context.Background(), p, path, positional[0], *optional, iso); err != nil {
			return err
		}

		cli.Remember(positional[0], *tmpl, *spec)

		return nil

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

		path, err := specFor(fs, positional[0], *tmpl, *spec)
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

		if _, err := cli.Snapshot(context.Background(), p, positional[0], positional[1]); err != nil {
			return err
		}

		cli.Inherit(positional[0], positional[1])

		return nil

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

		path, err := specFor(fs, positional[0], *tmpl, *spec)
		if err != nil {
			return err
		}

		if err := cli.Fork(context.Background(), p, path, positional[0], positional[1], *optional, iso); err != nil {
			return err
		}

		if *tmpl != "" || wasSet(fs, "spec") {
			cli.Remember(positional[1], *tmpl, *spec)
		} else {
			cli.Inherit(positional[0], positional[1])
		}

		return nil

	case "gc":
		fs := flag.NewFlagSet("gc", flag.ExitOnError)
		olderThan := fs.Duration("older-than", 0, "only offer artifacts older than this")
		force := fs.Bool("force", false, "actually delete; without it this only lists")
		snaps := fs.Bool("snapshots", false, "include snapshots, which are never swept by default")
		kind, socket, ns, isolation := backendFlags(fs)
		_ = fs.Parse(args)

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return cli.GC(context.Background(), p, os.Stdout, *olderThan, *force, *snaps)

	case "doctor":
		fs := flag.NewFlagSet("doctor", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "machine-readable")
		_ = fs.Parse(args)

		return cli.PrintReport(os.Stdout, cli.Doctor(context.Background()), *asJSON)

	case "exec":
		fs := flag.NewFlagSet("exec", flag.ExitOnError)
		kind, socket, ns, isolation := backendFlags(fs)
		tty := fs.Bool("t", false, "attach a terminal - for a shell, psql, redis-cli")

		// Parse first, then take positionals from what is left. flag stops at the first
		// non-flag argument, so `sbx exec -t br pg psql -U app` gives sbx the -t and hands
		// psql its own -U untouched - which is how docker and kubectl behave, and what
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

		// The age of the pins, not just the names. Every template image is pinned to a
		// digest so the sandbox you create is the one CI tested - which also means these
		// images stop getting updates until somebody refreshes them.
		if d := TemplatesRefreshed(); d != "" {
			fmt.Printf("\nimages pinned by digest, last refreshed %s\n", d)
		}

		return nil

	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		tmpl := fs.String("template", "postgres", "which built-in to start from: "+strings.Join(TemplateNames(), ", "))
		_ = fs.Parse(args)

		// To stdout, not to a file. `sbx init > sandbox.json` is explicit about what it
		// overwrites, and a command that silently writes into the working directory is one
		// people run once by accident and never trust again.
		path, err := MaterializeTemplate(*tmpl)
		if err != nil {
			return err
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		_, err = os.Stdout.Write(body)

		return err

	case "validate":
		fs := flag.NewFlagSet("validate", flag.ExitOnError)
		specFlag := fs.String("spec", defaultSpec, "path to sandbox.json")
		tmpl := fs.String("template", "", "check a built-in template instead")
		positional, rest := splitPositional(args, 1)
		_ = fs.Parse(rest)

		// A bare path is the shape a linter is invoked with: `sbx validate ./sandbox.json`.
		path := *specFlag
		if len(positional) > 0 {
			path = positional[0]
		}

		path, err := specPath(*tmpl, path)
		if err != nil {
			return err
		}

		return cli.Validate(os.Stdout, path)

	case "prewarm":
		fs := flag.NewFlagSet("prewarm", flag.ExitOnError)
		kind, socket, ns, isolation := backendFlags(fs)
		specPath := fs.String("spec", "", "pull the images this spec needs instead of every template's")
		_ = fs.Parse(args)

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		images := TemplateImages()

		if *specPath != "" {
			s, err := spec.LoadSpec(*specPath)
			if err != nil {
				return err
			}

			images = images[:0]

			for _, svc := range s.Services {
				if svc.Image != "" {
					images = append(images, svc.Image)
				}
			}

			sort.Strings(images)
		}

		return cli.Prewarm(context.Background(), p, os.Stdout, images)

	case "history":
		// No provider and no daemon: this reads a file. Asking what happened to a sandbox
		// has to work when docker is down, which is one of the times people ask.
		return cli.History(args, os.Stdout)

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

		if err := cli.Remove(context.Background(), p, positional[0]); err != nil {
			return err
		}

		cli.Forget(positional[0])

		return nil

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
		usage(os.Stdout)
		return nil

	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// runAdd is the agent-facing path: put a service nobody declared into an existing sandbox.
func runAdd(args []string) error {
	// `sbx add <sandbox> <service> --image ...` reads the way a person would write it, but
	// Go's flag package stops at the first non-flag argument, so the flags after the two
	// names would be silently ignored - and the command would fail claiming --image was
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

	// The same fallback env and fork use. Add reads the spec to respect ordinals reserved for
	// declared-but-not-yet-created services, and a sandbox made from --template has no
	// sandbox.json to find - so without this the reservations are silently not seen, and a
	// later `--optional` create can collide with the port this just took.
	specPath, err := specFor(fs, sandbox, "", *spec)
	if err != nil {
		return err
	}

	return cli.Add(context.Background(), p, specPath, sandbox, service, *image, cps, *health, env, *volume, extra, iso)
}

// splitPositional peels up to n leading non-flag arguments off the front.
func splitPositional(args []string, n int) (positional, rest []string) {
	i := 0
	for i < len(args) && i < n && !strings.HasPrefix(args[i], "-") {
		i++
	}

	return args[:i], args[i:]
}

// usage writes to w rather than always to stderr, because where it goes says whether
// something went wrong. `sbx --help` was asked for and belongs on stdout, so it can be
// piped into a pager or grepped; usage printed because a command was wrong is a diagnostic
// and belongs on stderr, where it will not be mistaken for output. Found by Homebrew's
// formula test, which read stdout, got nothing, and was right to fail.
func usage(w io.Writer) {
	fmt.Fprint(w, `sbx - per-branch sandboxes that sleep when nobody is using them

  sbx create <sandbox> [--spec sandbox.json | --template NAME] [--optional]
  sbx env    <sandbox> [--spec sandbox.json] [--shell posix|fish|powershell|cmd|json]
  sbx ready  <sandbox> [--timeout 90s]
  sbx add    <sandbox> <service> --image IMG --port 5432[,...] [--health CMD]
                                 [--env K=V,...] [--volume PATH] [ARGS...]
  sbx snapshot <sandbox> <name>                 save every service's filesystem
  sbx fork   <snapshot> <new-sandbox> [--spec]  a new sandbox from that state
  sbx gc     [--older-than DURATION] [--snapshots] [--force]  lists; deletes only with --force
  sbx doctor [--json]                           what this machine can and cannot do
  sbx prewarm [--spec sandbox.json]             pull images now so a create is not a download
  sbx init   [--template NAME]                  print a starter spec to stdout
  sbx validate [sandbox.json]                   check the spec; creates nothing
  sbx exec   [-t] <sandbox> <service> <command>...   -t attaches a terminal
  sbx logs   <sandbox> [service] [--tail N] [-f]   all services if none named
  sbx cp     <sandbox> <service> <src> <dst>    (inside path is prefixed with :)
  sbx url    <sandbox> <service> [--via cloudflared|ngrok|ssh]
  sbx list
  sbx history [sandbox] [--limit N] [--commands|--events] [--json]   what happened, and who did it
  sbx rm     <sandbox>
  sbx serve  [--idle 5m] [--socket PATH]
  sbx selftest [--provider ...] [--keep]     prove it works here (~9s warm)

Templates (no spec file needed): sbx templates

Backend (any command touching a sandbox): --provider docker|kubernetes  --namespace NS
                                          --isolation container|gvisor|kata

There is no start and no stop: connecting to a sandbox port wakes it, and idleness
sleeps it. Run one "sbx serve" per machine.
`)
}
