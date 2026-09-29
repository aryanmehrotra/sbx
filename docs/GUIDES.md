# Guides

One section per task. New to sbx? Start with [QUICKSTART.md](QUICKSTART.md). Every guide assumes
one `sbx serve --idle 5m &` per machine; [`deploy/`](../deploy/) has units to keep it running, and
[SELF-HOSTING.md](SELF-HOSTING.md) runs it on a server for a team.

## A database per branch

```sh
sbx create feature-x --template postgres
eval "$(sbx env feature-x)"        # sets PGHOST, PGPORT, DATABASE_HOST, DATABASE_PORT
PGPASSWORD=app psql -U app -d app -c 'select 1'   # this connection wakes it
```

Ports are assigned per sandbox, so read them from `sbx env`. `sbx templates` lists the built-in
specs; [examples/](../examples/) explains each. To add a service mid-task:
`sbx add feature-x cache --image redis:7-alpine --port 6379 --health 'redis-cli ping'`.
`sbx env` then prints `CACHE_HOST` and `CACHE_PORT` for it.

`--template browser` gives a headless Chrome that Playwright and Puppeteer drive over CDP at
`$CDP_HOST:$CDP_PORT` ([examples/browser](../examples/browser/)).

## Test fixtures in CI

`sbx with` creates a sandbox, waits until it answers, runs the command, then removes it, even on
failure. It exits with the command's status. `--keep` leaves the sandbox for inspection.

```sh
sbx with test-db --template postgres -- go test ./...
```

To keep a stack between jobs on a persistent runner, `sbx create` it once and run `sbx ready`
(blocks until every service answers) before each job. On a shared runner, name sandboxes after branch and job
([why](TROUBLESHOOTING.md#on-a-shared-or-persistent-ci-runner)).

## Seed once, fork many

Seed one sandbox, snapshot it (a copy of every service's `volume`), and fork copies. Seed with
`init` commands, which run once after the first healthy check, as in
[`examples/postgres`](../examples/postgres/sandbox.json).

```sh
sbx create main --template postgres   # seeded by init, once
sbx snapshot main golden
sbx fork golden agent-1        # its own copy and its own ports
sbx fork golden agent-2        # a write in one is invisible to the others
```

- To seed a running sandbox by hand, use `sbx cp` and `sbx exec`.
- A snapshot copies files, not memory, so forks start cold against warm data.
- It does not pause the service. Stop writes first if the copy must be exact
  ([why](TROUBLESHOOTING.md#a-fork-is-missing-the-write-i-just-made)).
- On docker in v0.14.0 it fails when any service has no `volume`
  ([workaround](TROUBLESHOOTING.md#sbx-snapshot-fails-the-source-is-empty-or-does-not-exist)).
- To save a running process's memory, use `sbx checkpoint <sandbox> <name>` and `sbx resume`.
  They use CRIU (a Linux checkpoint tool) and need Linux with podman.

## Share a preview link

`sbx url` opens a public tunnel (cloudflared, ngrok or ssh) to one service. It sleeps until
somebody opens the link.

```sh
sbx url my-branch web                             # https://....trycloudflare.com; --via ngrok to choose
SBX_FEATURES=waiting-page sbx serve --idle 5m &   # show a "starting" page during an HTTP wake
```

A per-pull-request workflow is in [examples/pr-preview](../examples/pr-preview/).

## Work inside a sandbox

To build or test inside a sandbox, declare a service with your tools that mounts your source:

```json
{ "version": 1,
  "services": { "dev": {
    "image": "golang:1.26-alpine", "ports": [7777],
    "args": ["sleep", "infinity"], "mounts": { ".": "/work" } } } }
```

```sh
sbx exec -t my-branch dev sh           # a shell in /work with your code; wakes it first
sbx exec my-branch dev go test ./...
```

- `args` keeps the container running. `ports` is required but can be any unused port.
- `"."` is the spec's directory, writable both ways. Docker only; on macOS and Windows it must be
  a path the docker VM shares, such as your home directory.

From an editor (preview feature): use an image that runs an ssh server, such as
`lscr.io/linuxserver/openssh-server`. `SBX_FEATURES=ssh sbx ssh feature-x --user dev` prints the
ssh and `code --remote` lines. Remote-SSH, JetBrains Gateway, `scp` and `rsync` wake the sandbox.
VS Code's Attach to Container and Remote-Tunnels do not.

From a devcontainer (preview feature):
`SBX_FEATURES=devcontainer sbx init --from-devcontainer . > sandbox.json`. It keeps the image or
build, ports, env, the workspace mount and the create commands, and lists what it skipped on
stderr. Features, `postStartCommand` and `postAttachCommand` are dropped; pass `remoteUser` as
`sbx ssh --user`. A `dockerComposeFile` is refused; add those services to `sandbox.json` yourself.

## Keep a sandbox awake, or limit where it can connect

sbx counts bytes through a service's ports to decide it is idle. Work that happens only inside,
such as compiling, sends none, so the sandbox can sleep mid-task.

| you want | write in the service |
|---|---|
| a longer idle window | `"idle": "30m"` |
| never sleep (holds its memory) | `"idle": "never"` |
| keep memory while asleep ([measured](BENCHMARKS.md#sbx-by-itself)) | `"on_idle": "freeze"` |
| reach only these hosts, and stay awake while calling them | `"egress_allow": ["api.anthropic.com", "pypi.org"]` |
| no outbound network | `"egress": "deny"` |

Egress is traffic going out of the sandbox. With `egress_allow`, HTTP(S) calls go through a
filtering proxy and count as activity. Other protocols do not, so give those a longer `idle`.
Tighten a running sandbox with `sbx egress agent-1 --deny '*.pastebin.com'`. Rules are in
[SPEC.md](SPEC.md#egress-the-network-a-service-may-reach).

## AI agents

A coding agent (Claude Code, Cursor, Codex) can create its own sandboxes. Paste the block below
into its instructions, register `sbx mcp` for MCP tools, or point OpenSandbox SDK code at sbx.

### Paste this into your agent's instructions

Put it in your project's `AGENTS.md` or `CLAUDE.md`.

```markdown
## Sandboxes (sbx)

When you need a service to do your work - a database to run a migration against, a redis, a
browser, a queue - create a sandbox for it. Do not `docker run` it, do not `docker compose up`,
and do not connect to a shared local database.

    sbx doctor --json                     # what this machine can do; run it first if anything is odd
    sbx list --json                       # what already exists
    sbx create <task> --template postgres # one for this task. Templates: sbx templates
    eval "$(sbx env <task>)"              # exports its addresses into your shell
    sbx ready <task>                      # block until it really answers, for scripts

There is no start and no stop. **Connecting is what wakes a service** - psql, a driver, a test
runner, curl - and idleness puts it back to sleep, using 0 B of RAM. Never hardcode a port: read
it from `sbx env`, which is the only place the real numbers exist. The variable names come from
the spec's `exports` (the postgres template gives `DATABASE_HOST` and `DATABASE_PORT`); a
service you `sbx add` gets `<SERVICE>_HOST` and `<SERVICE>_PORT`. Run `sbx env <task>` and read
them rather than assuming.

    sbx add <task> cache --image redis:7-alpine --port 6379 --health 'redis-cli ping'
    sbx exec <task> postgres psql -U app -d app -c 'select 1'
    sbx logs <task> postgres --tail 50
    sbx rm <task>                         # when the task is done. This deletes its data.

    sbx with <task> --template postgres -- <cmd>   # one-shot: create, run <cmd> with env set,
                                                   # then ALWAYS rm - even if <cmd> fails. Its
                                                   # exit code becomes sbx's. For a scoped test run.

Rules:
- One sandbox per task or branch, named after it. Do not reuse another task's.
- `sbx rm` only the sandbox you created. Somebody else's may be in use.
- Nothing is shared between sandboxes, so a migration in yours affects nothing else.
- If the addresses from `sbx env` refuse connections, no daemon is running: start
  `sbx serve --idle 5m &` once for the machine, then retry.
- If a command fails, run `sbx doctor` before guessing: it says whether docker is even up.
```

JSON output an agent can parse. Every refusal also names the field or flag behind it.

| command | gives |
|---|---|
| `sbx list --json` | every sandbox and service, with `awake`, `addresses`, `ref` |
| `sbx env <sandbox> --shell json` | the addresses as an object |
| `sbx doctor --json` | capabilities; each missing one says what it costs |
| `sbx history [sandbox] --json` | newline-delimited wakes, sleeps and changes |
| `sbx egress <sandbox> --json` | the network policy in force |

### MCP

MCP (Model Context Protocol) is how AI apps are given tools. `sbx mcp` has the same 19 tools as
OpenSandbox's own MCP server, with the same names, arguments and results. It needs the daemon to
serve the OpenSandbox API:

```sh
sbx serve --osb-addr 127.0.0.1:8080 &     # writes an API key to ~/.sbx/osb/key
claude mcp add sbx -- sbx mcp             # Claude Code; sbx mcp reads the key file itself
codex mcp add sbx -- sbx mcp              # Codex CLI
claude mcp add sbx -e SBX_OSB_KEY="$KEY" -- sbx mcp --url https://osb.example.dev   # another OpenSandbox server (sbx's own API is loopback only)
```

Cursor (`.cursor/mcp.json`), or any client that reads an `mcpServers` block:

```json
{ "mcpServers": { "sbx": { "command": "sbx", "args": ["mcp"], "env": { "SBX_OSB_URL": "http://127.0.0.1:8080" } } } }
```

| setting | where `sbx mcp` looks, in order |
|---|---|
| `--url` | `SBX_OSB_URL`, `OPEN_SANDBOX_DOMAIN`, `http://127.0.0.1:8080` |
| `--key` | `SBX_OSB_KEY`, `OPEN_SANDBOX_API_KEY`, then `~/.sbx/osb/key` for a loopback URL only |

| group | tools |
|---|---|
| sandbox | `sandbox_create` `sandbox_connect` `sandbox_kill` `sandbox_get_info` `sandbox_list` `sandbox_renew` `sandbox_healthcheck` `sandbox_get_metrics` `sandbox_get_endpoint` |
| commands | `command_run` `command_interrupt` |
| files | `file_read` `file_write` `file_delete` `file_search` `file_create_directories` `file_delete_directories` `file_move` `file_replace_contents` |

It replaces upstream's `opensandbox-mcp` with no other change. Any `sandbox_id` works in any tool,
and cancelling `command_run` interrupts the command. `file_read` and `file_write` take `utf-8` or
`latin-1` only. It speaks MCP `2025-11-25` back to `2024-11-05` and logs to stderr only.

### OpenSandbox SDKs

[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) is an open API standard for
AI-agent sandboxes, with SDKs in 5 languages. `sbx serve --osb-addr` serves its lifecycle API, so
SDK code runs on your machine with no account. Upstream's own test suite checks this
([test/osb](../test/osb/)).

```sh
sbx serve --osb-addr 127.0.0.1:8080 &
export OPEN_SANDBOX_DOMAIN=127.0.0.1:8080 OPEN_SANDBOX_API_KEY="$(cat ~/.sbx/osb/key)"
```

The key is required even on loopback ([why](../SECURITY.md#access-and-exposure)); `--osb-key`
sets your own. Python (`pip install opensandbox`, written against SDK 1.1.0):

```python
from opensandbox import SandboxSync

sandbox = SandboxSync.create("python:3.11-slim")   # reads OPEN_SANDBOX_DOMAIN and _API_KEY
try:
    run = sandbox.commands.run("python -c 'print(6 * 7)'")
    print(run.logs.stdout[0].text)                   # 42
finally:
    sandbox.destroy()                                # removes the sandbox, closes the client
```

Warm pools: `sbx serve --osb-pool IMAGE[=N]` keeps N sandboxes (default 8) ready, so a matching
create returns in milliseconds ([BENCHMARKS.md](BENCHMARKS.md#sbx-by-itself)). A create hits only if image, entrypoint, `resourceLimits`, ports, platform and `sbx.idle` match.
The pool uses the SDK defaults (entrypoint `["tail", "-f", "/dev/null"]`, `cpu: "1"`,
`memory: "2Gi"`), so a plain `create(image)` hits. Each miss is logged with the field that
differed. The pool is refused with `--provider firecracker` on a Mac or Windows. Other flags:
[CLI.md](CLI.md#sbx-serve).

## Stronger isolation with microVMs

A container shares the host's kernel. A microVM has its own, and
[Firecracker](https://firecracker-microvm.github.io/) is the microVM monitor AWS built for Lambda.
Use it for code you did not write.

```sh
sbx fc backend                                  # can this machine run microVMs, and how?
sbx serve --provider firecracker --idle 5m &
sbx create untrusted --spec sandbox.json --provider firecracker
```

- Linux with `/dev/kvm` runs it directly. An M3+ Mac or Windows 11 runs it in a helper VM.
- The image must run as root, and some fields are refused ([SPEC.md](SPEC.md#on---provider-firecracker)).
- What is tested where: [platform status](ARCHITECTURE.md#platform-status).
- Lighter options: `--isolation gvisor` (a user-space kernel) or `--isolation kata` (a lightweight
  VM per container). Each is refused with a reason when its runtime is missing.

## Kubernetes

The same `sandbox.json` runs in a cluster, through `kubectl` and your current context:
`sbx create my-branch --provider kubernetes --namespace sbx`, then `sbx env` with the same
`--provider`. Each service becomes a Deployment and a Service. Install the in-cluster daemon once from
[`deploy/activator.yaml`](../deploy/activator.yaml), after building its image
([`deploy/Dockerfile`](../deploy/Dockerfile)). Refused fields are in [SPEC.md](SPEC.md#provider-support).

## Deploy on a one-port platform

Some platforms run one container behind one HTTPS port. `sbx pack` makes a build context per
service, and `sbx connect` gives you local ports that tunnel to them.

<img src="connect.svg" width="820" alt="How sbx connect works: psql dials 127.0.0.1:5432 on your laptop, sbx connect carries that TCP stream over one authenticated HTTPS WebSocket to the single port the platform routes, and sbx serve --front hands it to a postgres that is never published.">

```sh
sbx pack --spec sandbox.json          # one build context per service, under sbx-pack/
# deploy sbx-pack/db/ and sbx-pack/cache/, each with SBX_CONNECT_TOKEN set

SBX_CONNECT_TOKEN_DB=... SBX_CONNECT_TOKEN_CACHE=... \
  sbx connect db=https://db.example.dev cache=https://cache.example.dev
#   db     ->  127.0.0.1:5432
#   cache  ->  127.0.0.1:6379
sbx ui --connect db=https://db.example.dev   # wake, sleep, limit, remove, logs, forward ports
```

To reach something only the container can route to, such as a private managed database, deploy
`sbx serve --connect-addr=":$PORT" --behind-proxy --front db=10.0.4.7:3306`.

- The token is the only protection, for everything the container can reach. Read [SECURITY.md](../SECURITY.md).
- Without a volume, most such platforms lose data when they replace the container.
- Every round trip crosses the internet.

## Coming from docker compose, Testcontainers or E2B

- docker compose: most fields map one to one ([SPEC.md](SPEC.md#coming-from-docker-compose)).
  Declare only the container port. There is no `up` or `down`.
- Testcontainers: wrap the test command, as in `sbx with test-db --template postgres -- npm test`.
- E2B, Daytona: sbx does not serve their APIs. Port the calls to the
  [OpenSandbox SDK](#opensandbox-sdks), which has the same shape ([COMPARISON.md](COMPARISON.md)).
