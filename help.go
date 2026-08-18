package main

// Per-command help.
//
// `sbx create --help` used to print Go's default flag dump: a list of flags, no synopsis, no
// mention that the command takes a sandbox name at all, and no example. Somebody who has to
// read that has already been told by the top-level help that this command exists - what they
// came for is how to type it, which was the one thing missing.
//
// So every flag set gets a synopsis and one real example. The example matters more than the
// prose: people copy it, change one word, and get on with their day.

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// help is what each command is for, in the reader's terms rather than the implementation's.
var help = map[string]struct{ synopsis, about, example string }{
	"create": {
		"sbx create <sandbox> [--spec sandbox.json | --template NAME] [--optional]",
		"Make a sandbox from a spec, or from a built-in template if you have no spec yet.\n" +
			"Its services start asleep; the first connection wakes them.",
		"sbx create feature-x --template postgres",
	},
	"env": {
		"sbx env <sandbox> [--shell posix|fish|powershell|cmd|json]",
		"Print the addresses of a sandbox's services, ready to eval into your shell.\n" +
			"--shell json is for anything that parses rather than sources.",
		`eval "$(sbx env feature-x)"`,
	},
	"ready": {
		"sbx ready <sandbox> [--timeout 90s]",
		"Block until every service really answers, then exit 0. For CI, where the next step\n" +
			"starts the moment this returns and a port that is merely open is not enough.",
		"sbx create ci-$GITHUB_RUN_ID --template postgres && sbx ready ci-$GITHUB_RUN_ID",
	},
	"add": {
		"sbx add <sandbox> <service> --image IMG --port N[,...] [--health CMD] [--env K=V,...]",
		"Put a service into a sandbox that its spec never declared. For an agent mid-task\n" +
			"that turns out to need a cache, without editing a file first.",
		"sbx add feature-x cache --image redis:7-alpine --port 6379 --health 'redis-cli ping'",
	},
	"snapshot": {
		"sbx snapshot <sandbox> <name>",
		"Save every service's filesystem under a name. Data only: processes start cold when\n" +
			"a fork of it is woken.",
		"sbx snapshot main golden",
	},
	"fork": {
		"sbx fork <snapshot> <new-sandbox>",
		"Make a sandbox from a snapshot. Seed and migrate once, then hand every agent its own\n" +
			"copy; a write in one is invisible to the others.",
		"sbx fork golden agent-1",
	},
	"gc": {
		"sbx gc [--older-than DURATION] [--snapshots] [--force]",
		"Reclaim volumes and images that dead sandboxes left behind. Lists what it would\n" +
			"remove and does nothing else unless you pass --force.",
		"sbx gc --older-than 168h --force",
	},
	"doctor": {
		"sbx doctor [--json]",
		"What this machine can and cannot do: docker or kubernetes, whether a daemon is\n" +
			"running, which isolation runtimes exist. Run it first when something is wrong.",
		"sbx doctor",
	},
	"prewarm": {
		"sbx prewarm [--spec sandbox.json]",
		"Pull the images now, so the first create is not a download. Useful in a CI image or\n" +
			"before a demo.",
		"sbx prewarm --spec sandbox.json",
	},
	"init": {
		"sbx init [--template NAME] [--yes]",
		"At a terminal: asks what this branch needs and writes sandbox.json.\n" +
			"Piped, it prints a spec to stdout instead, so `sbx init > sandbox.json` still works.",
		"sbx init",
	},
	"validate": {
		"sbx validate [sandbox.json]",
		"Check a spec and create nothing. Every refusal names the field it came from.",
		"sbx validate",
	},
	"exec": {
		"sbx exec [-t] <sandbox> <service> <command>...",
		"Run a command inside a service. -t attaches a terminal, for a shell or a REPL.",
		"sbx exec main postgres psql -U app -d app",
	},
	"logs": {
		"sbx logs <sandbox> [service] [--tail N] [-f]",
		"What a service printed. Every service if you name none. Reading logs does not wake\n" +
			"anything - watching a sandbox is not using it.",
		"sbx logs feature-x postgres -f",
	},
	"cp": {
		"sbx cp <sandbox> <service> <src> <dst>",
		"Copy a file in or out. A path inside the service is prefixed with a colon.",
		"sbx cp main postgres ./schema.sql :/tmp/schema.sql",
	},
	"pack": {
		"sbx pack [service] [--spec sandbox.json] [--out DIR]",
		"Build contexts for a platform that takes one container and one HTTP port.\n\n" +
			"A sandbox is normally a set of containers on a machine sbx controls. A PaaS gives\n" +
			"neither, so this writes the image that fits it: the workload exactly as it was, plus\n" +
			"sbx carrying its ports over the one port the platform routes. Deploy that image with\n" +
			"SBX_CONNECT_TOKEN set, then `sbx connect` turns it back into ordinary local ports.\n\n" +
			"The generated image starts the base image's own process, read out of the image rather\n" +
			"than guessed - so it works for whatever you packed, not just for postgres.",
		"sbx pack db --spec sandbox.json",
	},
	"connect": {
		"sbx connect <url> [<url> ...] [--sandbox NAME] [--port-offset N|LABEL=N]",
		"Local ports for a sandbox that is running somewhere else.\n\n" +
			"Point it at a deployment running `sbx serve --connect-addr` and it opens a listener\n" +
			"for every service that deployment fronts, on the SAME port numbers - so the `sbx env`\n" +
			"block from over there is correct here, and psql connects without knowing any of this\n" +
			"happened. Everything travels over the one HTTP endpoint the platform gives you.\n\n" +
			"Give it several. A platform that runs one container per service spreads a sandbox\n" +
			"across several deployments, and naming them - db=https://... - puts them back\n" +
			"together as one local port map. Each keeps the image its spec named, which is what\n" +
			"packing them into a single container would cost.\n\n" +
			"SBX_CONNECT_TOKEN holds the token. A named deployment reads SBX_CONNECT_TOKEN_<NAME>\n" +
			"first, because two deployments usually have two tokens. The URL must be https\n" +
			"unless it is on this machine, since the token is what the whole tunnel rests on -\n" +
			"SBX_CONNECT_INSECURE=1 waives that. Use --port-offset if this\n" +
			"machine already runs its own `sbx serve` and owns those ports, or if two deployments\n" +
			"front the same port: --port-offset replica=1000 moves only that one, and\n" +
			"--port-offset 1000,replica=2000 moves everything with an exception.",
		"SBX_CONNECT_TOKEN=... sbx connect db=https://db.example.dev cache=https://cache.example.dev",
	},
	"url": {
		"sbx url <sandbox> <service> [--via cloudflared|ngrok|ssh]",
		"A public link to a service, which wakes it when somebody opens it. For sharing a\n" +
			"branch preview with someone who cannot reach your laptop.",
		"sbx url feature-x nginx",
	},
	"list": {
		"sbx list [--json]",
		"Every sandbox on this machine, its services, whether each is awake, and the address\n" +
			"to connect to. --json is the same thing for something that parses rather than\n" +
			"reads - an agent asking what exists should not be reading column widths.",
		"sbx list --json",
	},
	"rm": {
		"sbx rm <sandbox>",
		"Delete a sandbox and its data. There is no undo, and a snapshot is how you keep it.",
		"sbx rm feature-x",
	},
	"serve": {
		"sbx serve [--idle 5m] [--socket PATH] [--connect-addr ADDR] [--front NAME=PORT]",
		"The daemon. It owns the ports `sbx env` hands out, wakes a sandbox when something\n" +
			"connects, and sleeps it after --idle. One per machine, not one per sandbox.\n" +
			"\n" +
			"--connect-addr serves the endpoint `sbx connect` dials, over one HTTP port -\n" +
			"which is all most platforms route. --front NAME=PORT offers a port that is not a\n" +
			"sandbox at all, so sbx can sit beside a workload in the same container and hand\n" +
			"that workload out; --behind-proxy is for when the platform terminates the TLS.\n" +
			"SBX_CONNECT_TOKEN must be set for any of it - it is the only thing standing\n" +
			"between the endpoint and whoever finds its URL.",
		"sbx serve --idle 5m &",
	},
	"selftest": {
		"sbx selftest [--keep]",
		"Create a sandbox, let it sleep to zero, wake it with a socket and check its data\n" +
			"survived. Proves the whole cycle on this machine in about nine seconds.",
		"sbx selftest",
	},
	"templates": {
		"sbx templates",
		"The built-in specs, and the date their images were last pinned by digest. Any of\n" +
			"them works with nothing on disk: sbx create <name> --template <template>.",
		"sbx templates",
	},
	"ui": {
		"sbx ui [--connect <url> ...] [--sandbox NAME]",
		"A live dashboard: every sandbox, whether it is awake, what it is costing in cpu and\n" +
			"memory, and what the daemon has been doing. Wake, sleep, read logs and remove from\n" +
			"the keyboard. Where there is no terminal it prints the table once instead.\n\n" +
			"`v` folds the table into one line per sandbox - what it holds, how many of its\n" +
			"services are up, and what the whole thing is costing - since a sandbox is what\n" +
			"every other command here names. Enter and s then act on all of it.\n\n" +
			"`a` shows the machine instead: every container on it, ours or not, and what each\n" +
			"is holding. What is using the memory is rarely answered by the sandboxes alone.\n\n" +
			"--connect watches a deployed sbx rather than this machine, taking the same URLs\n" +
			"and tokens as sbx connect - several of them merge into one screen. It is\n" +
			"read-only: a connect endpoint carries bytes and reports what it is fronting, so\n" +
			"waking, sleeping, capping and removing are refused there and say so.",
		"sbx ui --connect https://sbx.example.dev",
	},
	"history": {
		"sbx history [sandbox] [--limit N] [--commands|--events] [--json]",
		"What happened, and who did it: commands that changed something, and every wake and\n" +
			"sleep the daemon recorded. Reads a file, so it works when docker does not.",
		"sbx history feature-x",
	},
}

// newFlagSet builds a flag set that explains itself.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)

	fs.Usage = func() {
		h, ok := help[name]
		out := fs.Output()

		if !ok {
			fmt.Fprintf(out, "Usage: sbx %s\n", name)
		} else {
			fmt.Fprintf(out, "%s\n\n%s\n", h.synopsis, h.about)
		}

		// Only print a flags section when there are flags, so commands that take none do not
		// show an empty heading.
		var flags int

		fs.VisitAll(func(*flag.Flag) { flags++ })

		if flags > 0 {
			fmt.Fprintf(out, "\nFlags:\n")
			fs.PrintDefaults()
		}

		if ok && h.example != "" {
			fmt.Fprintf(out, "\nExample:\n  %s\n", h.example)
		}
	}

	return fs
}

// helpWanted reports whether args are asking for help rather than for work, so that commands
// which take positional arguments can print their own help instead of "missing sandbox name".
func helpWanted(args []string) bool {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			return true
		}
	}

	return false
}

// missing is the error for a command invoked without its arguments.
//
// The bare "missing sandbox name" it replaces was correct and left the reader to go and find
// the syntax somewhere else, which is a round trip an error message exists to prevent.
func missing(command, what string) error {
	h, ok := help[command]
	if !ok {
		return fmt.Errorf("missing %s", what)
	}

	return fmt.Errorf("missing %s\n     usage: %s\n     e.g.:  %s",
		what, strings.TrimPrefix(h.synopsis, "sbx "), h.example)
}

// commandHelp prints one command's help to stdout, for `sbx <command> --help`.
func commandHelp(name string) {
	fs := newFlagSet(name)
	fs.SetOutput(os.Stdout)
	fs.Usage()
}
