# Using sbx from AI coding agents

How to give a coding agent (Claude Code, Cursor, Codex and others) its own databases and services
with sbx. Paste the block below into your project's `AGENTS.md` or `CLAUDE.md`. The rest covers
recipes, the MCP server and the OpenSandbox API.

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

---

## What an agent needs to know

1. **There is no `up` and no `down`.** A service sleeps until something opens a socket to it.
   `sbx ready` exists because an open port is not yet a database that answers.
2. **Ports are assigned, not chosen.** `localhost:5432` may be another task's database. Read
   `sbx env`.
3. **One `sbx serve` per machine.** Without it, `sbx env` prints addresses nothing answers on.
   `sbx doctor` says so.
4. **A sandbox is the unit.** `env`, `rm`, `logs`, `snapshot` take a sandbox name; a sandbox holds
   several services.

## When the agent works inside the box

Idleness is measured on bytes through a service's port. An agent that edits and compiles inside a
box sends none, so the box looks idle and sleeps mid-task. Declare an allow-list: calls out go
through sbx's proxy, are limited to those hosts, and count as activity.

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

| you want | write |
|---|---|
| reach only these hosts, and stay awake while calling them | `"egress_allow": [...]` |
| no egress at all | `"egress": "deny"` |
| a longer idle window | `"idle": "30m"` |
| keep memory while asleep, thaw in milliseconds | `"on_idle": "freeze"` |
| never sleep | `"idle": "never"` (holds memory for the sandbox's lifetime) |

Change the policy of a running box with `sbx egress` (see [SPEC.md](SPEC.md#egress-the-network-a-service-may-reach)).

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
sbx cp golden postgres ./schema.sql :/tmp/schema.sql
sbx exec golden postgres psql -U app -d app -f /tmp/schema.sql
sbx snapshot golden ready

sbx fork ready attempt-1        # a full copy, its own ports
sbx fork ready attempt-2        # a write in one is invisible to the other
```

**Park and resume an agent's sandbox**

```sh
sbx sleep agent-42              # drop to 0 B now
sbx wake agent-42               # wake and wait until serving
sbx checkpoint agent-42 mid     # save memory and processes (Linux, podman)
sbx resume agent-42 mid
```

**A shell with the repository in it**

```json
{ "dev": { "image": "golang:1.26-alpine", "ports": [7777],
           "args": ["sleep", "infinity"], "mounts": { ".": "/work" } } }
```

```sh
sbx exec -t my-task dev sh            # a shell in /work with the code in it
sbx exec my-task dev go test ./...
```

`args` keeps the container alive. `ports` is required but unused. `"."` resolves against the
spec's directory. See [USE-CASES.md](USE-CASES.md#9--a-box-to-run-your-own-commands-in).

**Clean up:** `sbx rm my-task` removes the sandbox and its data. `sbx gc` lists what dead
sandboxes left; `--force` deletes it.

**Gated commands.** `sbx features` lists preview features. Turn one on per command:
`SBX_FEATURES=ssh sbx ssh <task>`.

### Machine-readable output

| command | gives |
|---|---|
| `sbx list --json` | every sandbox and service, with `awake`, `addresses`, `ref` |
| `sbx env <sandbox> --shell json` | the addresses as an object |
| `sbx doctor --json` | capabilities; each missing one says what it costs |
| `sbx history [sandbox] --json` | newline-delimited wakes, sleeps and changes |
| `sbx egress <sandbox> --json` | the network policy in force |
| `sbx validate` | checks a spec, creates nothing |

### Refusals an agent will hit

Every refusal names the field or flag it came from.

- **`build:`, `egress: "deny"` or `sbx url` on Kubernetes.** Name an `image`; use your CNI's
  NetworkPolicy; use an Ingress.
- **Clearing a limit on docker.** Docker cannot lift a ceiling on a running container. Recreate it.
- **`sbx connect` to a non-local `http://` URL.** The token would travel in the clear. Use https.
- **A firecracker service whose image `USER` is not root.** Use `--provider docker` or a root image.

---

## From an MCP client: `sbx mcp`

`sbx mcp` is an MCP server on stdio with **the same 19 tools as OpenSandbox's MCP server**: same
names, arguments and results. It talks to the OpenSandbox HTTP API, so it drives
`sbx serve --osb-addr` or a real OpenSandbox server.

```sh
sbx serve --osb-addr 127.0.0.1:8080 &     # the API; its key is written to ~/.sbx/osb/key
claude mcp add sbx -- sbx mcp             # Claude Code; reads that key file itself
codex mcp add sbx -- sbx mcp              # Codex CLI
claude mcp add sbx -e SBX_OSB_KEY="$KEY" -- sbx mcp --url https://osb.example.dev
```

Cursor, or any client that reads an `mcpServers` block:

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

| setting | order |
|---|---|
| `--url` | `SBX_OSB_URL`, `OPEN_SANDBOX_DOMAIN`, `http://127.0.0.1:8080` |
| `--key` | `SBX_OSB_KEY`, `OPEN_SANDBOX_API_KEY`, then `~/.sbx/osb/key` for a loopback URL only |

The upstream names mean swapping `opensandbox-mcp` for `sbx mcp` needs no other change.

| group | tools |
|---|---|
| sandbox | `sandbox_create` `sandbox_connect` `sandbox_kill` `sandbox_get_info` `sandbox_list` `sandbox_renew` `sandbox_healthcheck` `sandbox_get_metrics` `sandbox_get_endpoint` |
| commands | `command_run` `command_interrupt` |
| files | `file_read` `file_write` `file_delete` `file_search` `file_create_directories` `file_delete_directories` `file_move` `file_replace_contents` |

Differences from upstream's server:

- **Any `sandbox_id` works in any tool.** No `connect_if_missing` needed (still accepted).
- **Cancelling `command_run` interrupts the command** in the sandbox.
- **`file_read` / `file_write` take `utf-8` or `latin-1` only.** Use `iconv` via `command_run`
  for others.

It speaks MCP `2025-11-25` back to `2024-11-05` and logs to stderr only.

---

## From OpenSandbox SDK code

`sbx serve --osb-addr` serves the OpenSandbox lifecycle API, so OpenSandbox's SDKs work against
it unchanged.

```sh
sbx serve --osb-addr 127.0.0.1:8080 &
export OPEN_SANDBOX_DOMAIN=127.0.0.1:8080
export OPEN_SANDBOX_PROTOCOL=http
export OPEN_SANDBOX_API_KEY="$(cat ~/.sbx/osb/key)"
```

**The key is always required**, loopback included: on colima and Docker Desktop every container
can reach the host's `127.0.0.1`. Set your own with `--osb-key` or `SBX_OSB_KEY`; otherwise one is
generated once into `~/.sbx/osb/key`. `--osb-insecure-no-key` turns it off (loopback only).

**Warm pools.** `sbx serve --osb-pool IMAGE[=N]` keeps N (default 8) sandboxes ready, so a
create is answered in milliseconds. Only an identical create hits the pool. Members are keyed
on image, entrypoint and `resourceLimits` (plus ports, platform and `sbx.idle`). The pool uses
the SDK defaults: entrypoint `["tail", "-f", "/dev/null"]`, `cpu: "1"`, `memory: "2Gi"`.
A plain SDK `create(image)` hits it. A request that omits `resourceLimits` or sets another
entrypoint goes cold, and the daemon log says why:
`pool miss for image python:3.11-slim: resourceLimits.cpu is unset, the pool's is "1"`.

Operator flags (`--osb-host-paths`, `--osb-pool-freeze`, firecracker rules) are in
[CLI.md](CLI.md#sbx-serve) and [SECURITY.md](../SECURITY.md).

---

## More

Services deployed elsewhere: [USE-CASES.md](USE-CASES.md#8--a-sandbox-that-is-not-on-your-laptop).
When stuck, run `sbx doctor`, then read [TROUBLESHOOTING.md](TROUBLESHOOTING.md) (symptom →
cause → fix). Every command and variable is in [CLI.md](CLI.md).
