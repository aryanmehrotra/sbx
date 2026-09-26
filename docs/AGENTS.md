# sbx for coding agents

> **Short version:** paste the block below into your project's `AGENTS.md`, `CLAUDE.md`, or
> whatever your assistant reads. The rest is why each line is there, plus the commands an agent
> reaches for.

An agent on a branch keeps needing somewhere to put a database. The usual answers fail later:
`docker run postgres` leaves a container that outlives the task and belongs to nobody; a shared
database turns a migration tried on one branch into one done to all. sbx gives the task its own,
then takes it away.

---

## The block to paste

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
runner, curl - and idleness puts it back to sleep at 0 B. Never hardcode a port: read it from
`sbx env`, which is the only place the real numbers exist. The variable names come from the
spec's `exports` (the postgres template gives `DATABASE_HOST` and `DATABASE_PORT`), so run
`sbx env <task>` and read them rather than assuming.

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
- If a command fails, run `sbx doctor` before guessing: it says whether docker is even up.
```

Everything below is for the agent that needs more than the common case.

---

## The four things that are not obvious

**1. There is no `up` and no `down`.** A service sleeps until something opens a socket to it —
zero memory, not a stopped container to remember. Connecting is the start; an agent never
"starts" anything. `sbx ready` exists because a script's next line runs immediately, and an open
port is not yet a database that answers.

**2. The ports are assigned, not chosen.** Each sandbox gets a block, so two branches can both
have "a postgres" without knowing about each other. `sbx env` prints the real ones; writing
`localhost:5432` into a config hardcodes another task's database.

**3. One daemon per machine.** `sbx serve` owns every sandbox's ports. If it is not running,
`sbx env` prints addresses nothing answers on — indistinguishable from a broken service.
`sbx doctor` says so in one line.

**4. A sandbox is the unit.** `sbx env`, `sbx rm`, `sbx logs` and `sbx snapshot` all take a
sandbox name, and a sandbox holds several services. In `sbx ui`, `v` shows that shape.

---

## When you are the workload, not the client

Everything above assumes the sandbox holds a service and you are outside it, dialling in. The
other shape is a box **you work inside** — you edit files, compile, and call an API — and nothing
ever dials you.

That shape breaks the rule the rest of sbx runs on. Idleness is measured on bytes through a
service's port, and you send none of them: reading a file is invisible, a compile is invisible.
On that measure your box looks idle from the moment you start working, and the window closes
mid-task.

**One of the things you do is visible.** Declare an allow-list and your calls out go through a
proxy sbx runs, so they count as activity:

```json
{
  "version": 1,
  "services": {
    "agent": {
      "image": "python:3.12",
      "ports": [7777],
      "egress_allow": ["api.anthropic.com", "pypi.org", "github.com"],
      "idle": "10m"
    }
  }
}
```

The box reaches those three hosts and nothing else — there is no route around the proxy, so the
list is enforced rather than advisory — and it stays awake while it is calling them, then sleeps
ten minutes after you stop.

| you want | write |
|---|---|
| reach only these hosts, and stay awake while calling them | `"egress_allow": [...]` |
| no egress at all | `"egress": "deny"` |
| a longer window | `"idle": "30m"` |
| never sleep, whatever happens | `"idle": "never"` |

Reach for `idle: "never"` **only** when there is no allow-list to use, or when the work does not
go over HTTP. It holds the box's memory for as long as the sandbox exists, which is the cost
sleeping was for. The stamp reaches services on the same sandbox bridge that declared an
allow-list of their own; a plain database beside your box still sleeps on its own timer.

## Some commands are behind a gate

`sbx features` lists what this build gates and what is currently on. A gated command tells you how
to turn itself on rather than pretending not to exist, and there is deliberately no `all`.

```sh
sbx features
SBX_FEATURES=ssh sbx ssh <task>
```

---

## Recipes

**Try a migration without touching anything shared**

```sh
sbx create migrate-users --template postgres
eval "$(sbx env migrate-users)"
psql -h "$DATABASE_HOST" -p "$DATABASE_PORT" -U app -d app -f migrations/007.sql
```

**Seed once, then give every attempt its own copy**

```sh
sbx create golden --template postgres
sbx exec golden postgres psql -U app -d app -f schema.sql
sbx snapshot golden ready

sbx fork ready attempt-1        # a full copy, its own ports
sbx fork ready attempt-2        # a write in one is invisible to the other
```

This changes what is affordable: the cost of a per-task database is the seeding, not the
container — pay it once.

**A service the spec never mentioned, mid-task**

```sh
sbx add my-task cache --image redis:7-alpine --port 6379 --health 'redis-cli ping'
```

**Reproduce a bug against the exact version**

```sh
sbx create repro --spec sandbox.json   # pinned images, committed with the repo
sbx ready repro
```

**Somewhere to run your own commands, with the repository in it**

`sbx exec` drops you into the named service's container, but a postgres image has psql and no
git. To run a build, script or test suite in the sandbox, declare a service that holds the
toolchain and mounts the source:

```json
{ "dev": { "image": "golang:1.26-alpine", "ports": [7777],
           "args": ["sleep", "infinity"], "mounts": { ".": "/work" } } }
```

```sh
sbx exec -t my-task dev sh            # a shell in /work with the code in it
sbx exec my-task dev go test ./...
```

`args` keeps it alive - a container whose command exits is gone. `ports` is required by the
spec but unused here: everything else in sbx is reached over a socket; this service is the
exception. A relative mount resolves against the spec's directory, so `"."` is the repository.
→ [USE-CASES.md](USE-CASES.md#9--a-box-to-run-your-own-commands-in)

**Find out what is going on**

```sh
sbx list --json                        # what exists, awake or not
sbx logs my-task postgres --tail 100   # reading logs does not wake anything
sbx history my-task --json             # every wake, sleep and change, newline-delimited
sbx ui                                 # live, if there is a terminal. v groups by sandbox
```

**Clean up**

```sh
sbx rm my-task                         # the sandbox and its data
sbx gc                                 # what dead sandboxes left. Lists; --force deletes
```

---

## From an MCP client: `sbx mcp`

Everything above is a CLI an agent shells out to. An agent whose harness speaks the Model
Context Protocol can have the sandbox as tools instead: `sbx mcp` is an MCP server on stdin and
stdout that offers **the same nineteen tools as OpenSandbox's own MCP server** — same names, same
arguments, same result fields — so a prompt or a config written for one works with the other.

It is a client of the OpenSandbox HTTP API, not of sbx's internals: point it at
`sbx serve --osb-addr` or at a real OpenSandbox server and it behaves the same.

```sh
sbx serve --osb-addr 127.0.0.1:8080 &     # the OpenSandbox API; its key goes in ~/.sbx/osb/key
claude mcp add sbx -- sbx mcp             # Claude Code - reads that key file itself
claude mcp add sbx -e SBX_OSB_KEY="$KEY" -- sbx mcp --url https://osb.example.dev
```

Cursor, or anything else that reads an `mcpServers` block:

```json
{
  "mcpServers": {
    "sbx": {
      "command": "sbx",
      "args": ["mcp"],
      "env": { "SBX_OSB_URL": "http://127.0.0.1:8080" }
    }
  }
}
```

`--url` falls back to `SBX_OSB_URL`, then `OPEN_SANDBOX_DOMAIN`, then `http://127.0.0.1:8080`;
`--key` to `SBX_OSB_KEY`, then `OPEN_SANDBOX_API_KEY`, then - for a loopback `--url` only - the
key `sbx serve` generated in `~/.sbx/osb/key`. The second name in each pair is the one
upstream's server reads, so swapping `opensandbox-mcp` for `sbx mcp` needs no other change. The
key file is never sent to a server that is not on this machine.

`sbx serve --osb-addr` always requires a key, loopback included: on colima and Docker Desktop
every container reaches the host's `127.0.0.1` through the VM's gateway, so loopback does not
keep sandboxes away from the API. Pass `--osb-key` (or `SBX_OSB_KEY`) to choose it; otherwise
one is generated once and reused. `--osb-insecure-no-key` turns it off, loopback only, and says
why that is dangerous every time it starts.

**Warm pools** (`sbx serve --osb-pool IMAGE[=N]`) answer a create in milliseconds, but only a
create whose container would be identical: members are keyed on **image, entrypoint and
resourceLimits** (plus ports, platform and `sbx.idle`). A pool is built with what the SDKs send
when those are left out - entrypoint `["tail", "-f", "/dev/null"]`, `cpu: "1"`, `memory: "2Gi"` -
so a plain SDK `create(image)` hits it, and a hand-written request that omits `resourceLimits`
or sets another entrypoint goes cold. The daemon log says so, once per distinct miss every ten
minutes: `pool miss for image python:3.11-slim: resourceLimits.cpu is unset, the pool's is "1"`.

| group | tools |
|---|---|
| sandbox | `sandbox_create` `sandbox_connect` `sandbox_kill` `sandbox_get_info` `sandbox_list` `sandbox_renew` `sandbox_healthcheck` `sandbox_get_metrics` `sandbox_get_endpoint` |
| commands | `command_run` `command_interrupt` |
| files | `file_read` `file_write` `file_delete` `file_search` `file_create_directories` `file_delete_directories` `file_move` `file_replace_contents` |

Where it differs from upstream's server, on purpose:

- **Any `sandbox_id` works in any tool.** Upstream refuses an id it did not create or connect to
  in this session unless the call passes `connect_if_missing`; sbx resolves it on demand, so
  "list, then run a command in one" is two calls, not three and an error. The argument is still
  accepted.
- **Cancelling `command_run` stops the command.** The client's cancel reaches the sandbox as an
  interrupt, rather than leaving the command running with nobody reading it.
- **`file_read` / `file_write` take `utf-8` or `latin-1`.** Go's standard library carries no
  other codecs; for anything else, `iconv` through `command_run`.

It speaks MCP `2025-11-25`, `2025-06-18`, `2025-03-26` and `2024-11-05`, answers `ping` and
cancellation while a tool is running, reports `sandbox_create` and `command_run` progress to a
client that asks for it, and writes nothing to stdout but protocol — its own log is on stderr.

---

## Machine-readable surfaces

An agent should parse these rather than the human tables:

| | |
|---|---|
| `sbx list --json` | every sandbox and service, with `awake`, `addresses`, `ref` |
| `sbx env <sandbox> --shell json` | the addresses as an object, for anything that is not a shell |
| `sbx doctor --json` | capabilities, each absent one saying what its absence costs |
| `sbx history [sandbox] --json` | newline-delimited records: wakes, sleeps, and commands that changed something |

`sbx validate` checks a spec and creates nothing — the cheap way to check a file just written.

---

## What it will refuse, and why

Knowing these saves an agent a turn spent fighting them:

- **`build:` in a spec, on kubernetes.** Building in a cluster needs a registry the nodes can
  pull from, which sbx cannot assume. Name an `image` instead.
- **`egress: "deny"` on kubernetes.** The equivalent is a NetworkPolicy, which only some CNIs
  enforce; applying it and reporting success would leave a service wide open against the spec.
- **`sbx url` on kubernetes.** It points at an Ingress rather than inventing a tunnel to a pod.
- **Clearing a limit on docker.** Docker cannot remove a ceiling from an existing container;
  recreate the sandbox. A cluster can, and is allowed to.
- **`sbx connect` to an `http://` URL not on this machine.** The token would cross the network in
  the clear.
- **`sbx serve --provider firecracker --osb-addr` as a user without CAP_NET_ADMIN.** It runs as
  root: every microVM is a tap on a bridge the daemon makes and guards with iptables rules, and a
  daemon that could not do that would accept every create and fail it on the tap. The VMM then
  is started through Firecracker's jailer: chrooted, as its own non-root uid, in its own cgroup.
- **`sbx serve --provider firecracker --osb-addr` with `SBX_FC_JAILER=off`.** Every VMM would be
  unconfined root, which an API handing VMs to callers must not do by default; pass
  `--osb-insecure-no-jailer` to accept it (SECURITY.md).
- **`sbx serve --provider firecracker --osb-addr --osb-insecure-no-key` on a Mac or Windows.** The
  API is served in the helper VM and forwarded to this machine's loopback, which containers on a
  VM-backed engine reach, so it is keyed or not served; the front also refuses to start unless the
  in-VM API answers 401 without the key. `--osb-pool` is refused there too (not carried into the
  VM yet) - run creates cold, or the pool on Linux.
- **A microVM whose host guard cannot be installed** (no iptables, a refused rule). It fails closed
  rather than boot a guest that can reach every host service; `--fc-firewall=unmanaged` hands the
  host firewall to the operator.

Every refusal names the field or the flag it came from, so the message is usually the fix.

---

## When the machine is not the one running the services

`sbx pack` writes a deployable image per service, and `sbx connect` turns those deployments back
into ordinary local ports - several at once, as one port map:

```sh
sbx connect db=https://db.example.dev cache=https://cache.example.dev
#   db     ->  127.0.0.1:5432
#   cache  ->  127.0.0.1:6379
```

Then `psql -h 127.0.0.1 -p 5432` reaches a database elsewhere without knowing anything happened.
→ [USE-CASES.md](USE-CASES.md#8--a-sandbox-that-is-not-on-your-laptop)

---

## If something is wrong

Run `sbx doctor` first. Most of what an agent hits is one of four things — the daemon not
running, most often by a distance.

→ [TROUBLESHOOTING.md](TROUBLESHOOTING.md) is written as symptom → cause → fix — point an agent
at it directly when it is stuck.
