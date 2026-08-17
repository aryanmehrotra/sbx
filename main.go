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
	"os/exec"
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
	"github.com/aryanmehrotra/sbx/internal/ui"
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
	// `sbx serve --help` and `sbx history --help` reach flag sets that live in other packages
	// and would otherwise print Go's bare dump, so they are answered here instead.
	//
	// Only in first position. `sbx exec main postgres psql --help` is asking psql for its
	// help, not sbx for its own, and a command that swallows that is one you cannot use to
	// run anything with a --help flag.
	if len(args) > 0 && helpWanted(args[:1]) {
		if _, ok := help[cmd]; ok {
			commandHelp(cmd)

			return nil
		}
	}

	switch cmd {
	case "create":
		fs := newFlagSet("create")
		spec := fs.String("spec", defaultSpec, "path to sandbox.json")
		tmpl := fs.String("template", "", "use a built-in spec instead: "+strings.Join(TemplateNames(), ", "))
		optional := fs.Bool("optional", false, "include services marked optional")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 1)
		_ = fs.Parse(rest)

		if len(positional) < 1 {
			return missing(cmd, "sandbox name")
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
		fs := newFlagSet("env")
		spec := fs.String("spec", defaultSpec, "path to sandbox.json")
		tmpl := fs.String("template", "", "use a built-in spec instead")
		shell := fs.String("shell", "", "posix | fish | powershell | cmd | json; detected if unset")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 1)
		_ = fs.Parse(rest)

		if len(positional) < 1 {
			return missing(cmd, "sandbox name")
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
		fs := newFlagSet("ready")
		timeout := fs.Duration("timeout", 90*time.Second, "give up after this long")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 1)
		_ = fs.Parse(rest)

		if len(positional) < 1 {
			return missing(cmd, "sandbox name")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return cli.Ready(context.Background(), p, positional[0], *timeout)

	case "add":
		return runAdd(args)

	case "snapshot":
		fs := newFlagSet("snapshot")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 2)
		_ = fs.Parse(rest)

		if len(positional) < 2 {
			return missing("snapshot", "arguments")
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
		fs := newFlagSet("fork")
		spec := fs.String("spec", defaultSpec, "path to the spec the snapshot came from")
		tmpl := fs.String("template", "", "use a built-in spec instead: "+strings.Join(TemplateNames(), ", "))
		optional := fs.Bool("optional", false, "include services marked optional")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 2)
		_ = fs.Parse(rest)

		if len(positional) < 2 {
			return missing("fork", "arguments")
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
		fs := newFlagSet("gc")
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
		fs := newFlagSet("doctor")
		asJSON := fs.Bool("json", false, "machine-readable")
		_ = fs.Parse(args)

		return cli.PrintReport(os.Stdout, cli.Doctor(context.Background()), *asJSON)

	case "exec":
		fs := newFlagSet("exec")
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
			return missing("exec", "arguments")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return cli.Exec(context.Background(), p, positional[0], positional[1], positional[2:], *tty)

	case "logs":
		fs := newFlagSet("logs")
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
		fs := newFlagSet("cp")
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

	case "pack":
		fs := newFlagSet("pack")
		specPath := fs.String("spec", "sandbox.json", "the spec to pack")
		out := fs.String("out", "sbx-pack", "directory to write the build contexts into")
		positional, rest := splitPositional(args, 1)
		_ = fs.Parse(rest)

		service := ""
		if len(positional) > 0 {
			service = positional[0]
		}

		return cli.Pack(context.Background(), cli.PackOptions{
			Spec:    *specPath,
			Service: service,
			Out:     *out,
			Version: version,
			Inspect: cli.InspectImage(dockerCLI),
			Out2:    os.Stdout,
		})

	case "connect":
		fs := newFlagSet("connect")
		offset := fs.String("port-offset", "",
			"add this to every local port, label=N for one deployment, or both (1000,replica=2000)")
		only := multiFlag{}
		fs.Var(&only, "sandbox", "only this sandbox; repeatable")
		positional, rest := splitPositional(args, len(args))
		_ = fs.Parse(rest)

		// Go's flag package stops at the first non-flag argument, so a URL written after a flag
		// is left behind in fs.Args() rather than parsed. Every other command here takes a fixed
		// number of names and splits them off the front, which makes that impossible; this one
		// takes as many as you give it, so `sbx connect db=… --port-offset 1000 cache=…` quietly
		// connected db alone and said nothing about cache.
		//
		// Refused rather than ignored, because a port map with a hole in it is the failure this
		// whole command is built to avoid: the missing port is left to whatever else answers on
		// it - often this machine's own `sbx serve` - and the caller reaches a local sandbox
		// believing it reached the remote one.
		if fs.NArg() > 0 {
			return fmt.Errorf("%s came after a flag, where it would have been ignored\n"+
				"     flags go last, so that every deployment is seen:\n"+
				"       sbx connect %s ...",
				strings.Join(fs.Args(), ", "),
				strings.Join(append(append([]string{}, positional...), fs.Args()...), " "))
		}

		if len(positional) < 1 {
			return fmt.Errorf("usage: sbx connect <url> [<url> ...] [--sandbox NAME] [--port-offset N]\n" +
				"     each url is a deployment running `sbx serve --connect-addr`, and\n" +
				"     SBX_CONNECT_TOKEN must hold the token it was given. Several deployments\n" +
				"     become one local port map: sbx connect db=https://... cache=https://...")
		}

		every, byLabel, err := daemon.ParseOffsets(*offset)
		if err != nil {
			return err
		}

		endpoints := make([]daemon.Endpoint, 0, len(positional))

		for _, arg := range positional {
			label, raw := daemon.SplitEndpoint(arg)

			// A named deployment gets its own variable, falling back to the shared one. Two
			// deployments usually have two tokens, and the alternative to naming them is
			// reusing one secret across both.
			token := os.Getenv("SBX_CONNECT_TOKEN")
			if label != "" {
				if t := os.Getenv(daemon.TokenVar(label)); t != "" {
					token = t
				}
			}

			endpoints = append(endpoints, daemon.Endpoint{Label: label, URL: raw, Token: token})
		}

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		return daemon.Connect(ctx, daemon.ClientOptions{
			Endpoints: endpoints,
			Sandbox:   only,
			Offset:    every,
			Offsets:   byLabel,
			Out:       os.Stdout,
		})

	case "url":
		fs := newFlagSet("url")
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
		fs := newFlagSet("init")
		tmpl := fs.String("template", "postgres", "which built-in to start from: "+strings.Join(TemplateNames(), ", "))
		yes := fs.Bool("yes", false, "take every default and ask nothing")
		_ = fs.Parse(args)

		// Guided at a terminal; unchanged in a pipeline.
		//
		// `sbx init > sandbox.json` prints the spec to stdout exactly as it always has - it
		// is in scripts and in this project's own docs, and a prompt appearing in a pipeline
		// is worse than the first-run problem the guided version fixes. Naming --template
		// explicitly also means you know what you want and are not asking to be asked.
		chosen := false

		fs.Visit(func(f *flag.Flag) {
			if f.Name == "template" {
				chosen = true
			}
		})

		return runInit(*tmpl, chosen, *yes, os.Stdout)

	case "validate":
		fs := newFlagSet("validate")
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
		fs := newFlagSet("prewarm")
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

	case "ui", "dash", "dashboard":
		fs := newFlagSet("ui")
		kind, socket, ns, isolation := backendFlags(fs)
		_ = fs.Parse(args)

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return ui.Run(context.Background(), ui.Options{
			Provider: p,
			Version:  version,
			Repo:     "aryanmehrotra/sbx",
		}, os.Stdout)

	case "history":
		// No provider and no daemon: this reads a file. Asking what happened to a sandbox
		// has to work when docker is down, which is one of the times people ask.
		return cli.History(args, os.Stdout)

	case "list":
		fs := newFlagSet("list")
		kind, socket, ns, isolation := backendFlags(fs)
		_ = fs.Parse(args)

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		return cli.List(context.Background(), p)

	case "rm":
		fs := newFlagSet("rm")
		kind, socket, ns, isolation := backendFlags(fs)
		positional, rest := splitPositional(args, 1)
		_ = fs.Parse(rest)

		if len(positional) < 1 {
			return missing(cmd, "sandbox name")
		}

		p, _, err := resolve(*kind, *socket, *ns, *isolation)
		if err != nil {
			return err
		}

		// Checked here rather than trusting the backend's own refusal: a provider reports
		// "no sandbox" without knowing which ones do exist, and a typo is the usual reason
		// somebody is reading this.
		ctx := context.Background()

		if units, err := p.List(ctx, positional[0]); err == nil && len(units) == 0 {
			return cli.UnknownSandbox(ctx, p, positional[0])
		}

		if err := cli.Remove(ctx, p, positional[0]); err != nil {
			return err
		}

		cli.Forget(positional[0])

		return nil

	case "selftest":
		fs := newFlagSet("selftest")
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

	fs := newFlagSet("add")
	image := fs.String("image", "", "container image (required)")
	ports := fs.String("port", "", "comma-separated container ports (required)")
	health := fs.String("health", "", "command run inside the container to test readiness")
	volume := fs.String("volume", "", "container path to persist")
	envs := fs.String("env", "", "comma-separated KEY=VALUE pairs")
	spec := fs.String("spec", defaultSpec, "path to sandbox.json, whose reservations are respected")
	kind, socket, ns, isolation := backendFlags(fs)
	_ = fs.Parse(rest)

	if len(positional) < 2 {
		return missing("add", "arguments")
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

A sandbox is a named group of services - a postgres, a redis, a browser - that belong to one
branch, task or agent. Nothing is started or stopped by hand: connecting to a port wakes it,
and idleness sleeps it back to 0 B.

Start here
  sbx doctor                                    what this machine can and cannot do
  sbx init                                      pick a template, write sandbox.json, go
  sbx serve  [--idle 5m]                        the daemon. One per machine, not per sandbox
  sbx selftest                                  prove the whole cycle works here (~9s warm)

Every day
  sbx create <sandbox> [--template NAME]        make one. --optional includes optional services
  sbx env    <sandbox> [--shell posix|json]     its addresses, as exports or JSON
  sbx list                                      every sandbox, its services and their state
  sbx ui                                        the same, live, with cpu and memory
  sbx rm     <sandbox>                          delete it and its data

While you work
  sbx logs   <sandbox> [service] [-f]           all services if none named
  sbx exec   [-t] <sandbox> <service> <cmd>...  -t attaches a terminal
  sbx cp     <sandbox> <service> <src> <dst>    a path inside is prefixed with :
  sbx add    <sandbox> <service> --image IMG --port N   a service the spec never declared
  sbx url    <sandbox> <service>                a public link that wakes it on open
  sbx connect <url>...                          local ports for DEPLOYED sbx, several as one map
  sbx pack   [service] [--spec F]               build contexts for a platform that takes one container
  sbx ready  <sandbox> [--timeout 90s]          block until it is really serving. For CI

Data
  sbx snapshot <sandbox> <name>                 save every service's filesystem
  sbx fork     <snapshot> <new-sandbox>         a new sandbox from that state
  sbx gc       [--force]                        reclaim what dead sandboxes left. Lists by default

Finding out
  sbx history  [sandbox] [--events|--commands]  what happened, and who did it
  sbx templates                                 the built-in specs, and when they were pinned
  sbx validate [sandbox.json]                   check a spec without creating anything
  sbx prewarm  [--spec sandbox.json]            pull images now, so a create is not a download

Any command touching a sandbox also takes:
  --provider docker|kubernetes   --namespace NS   --isolation container|gvisor|kata

`+"`sbx <command> --help`"+` explains one command.
`)
}

// multiFlag is a string flag that may be given more than once.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)

	return nil
}

// dockerCLI runs the docker CLI, for the few places that need to ask it something rather than
// go through a provider - `sbx pack` reads an image's entrypoint before any sandbox exists.
func dockerCLI(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return strings.TrimSpace(string(out)), nil
}
