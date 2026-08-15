# sbx

[![CI](https://github.com/aryanmehrotra/sbx/actions/workflows/ci.yaml/badge.svg)](https://github.com/aryanmehrotra/sbx/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8)](go.mod)
[![dependencies](https://img.shields.io/badge/dependencies-0-3fb950)](go.mod)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**One Go binary. Every branch, task or agent gets its own Postgres, Redis or browser — asleep
at 0 B, awake in 191 ms the moment something connects to it.**

<img src="docs/demo.svg" alt="A terminal running sbx selftest: a sandbox is created, sleeps to zero, is woken by a socket in 251 ms, and its data survives." width="860">

```sh
sbx serve --idle 5m &                          # once per machine, not per sandbox
sbx create my-branch --template postgres       # 492 ms once the image is local
eval "$(sbx env my-branch)"                    # it remembers what it was made from
psql -U app -d app                             # this wakes it
```

There is no `sbx start` and no `sbx stop`. **Opening a socket is the only signal**, so `psql`,
a connection pool, Playwright and a test runner all wake it without knowing it exists.

---

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | sh
# or
go install github.com/aryanmehrotra/sbx@latest
```

One static binary, **nothing outside Go's standard library**. darwin, linux, freebsd and
windows on amd64 and arm64; Windows means WSL2.

Then run the daemon once — it owns the ports `sbx env` hands out, so nothing works without
it. [`deploy/`](deploy/) has a launchd plist and a systemd unit, both running as you, not
root. Check the machine first, and prove the whole cycle on it — about 9 s once images are
local, longer on a first run that has to pull them:

```sh
sbx doctor       # what this host can and cannot do
sbx selftest     # create, sleep to zero, wake on a socket, data intact
```

---

## Use it

Five situations, differing mostly in *who types the commands*. This is the **how**;
[USE-CASES.md](docs/USE-CASES.md) is the **why** — the problem each solves, and the numbers.

### You, with three branches open

The case it was built for. One spec in the repo, one sandbox per branch:

```sh
git switch feature-x
sbx create feature-x                  # reads ./sandbox.json
eval "$(sbx env feature-x)"           # DATABASE_PORT=20002, REDIS_PORT=20003…
npm test                              # your tooling, unchanged
```

Switch away and it sleeps to 0 B on its own. Switch back and the first query wakes it.

### An agent, mid-task

**No SDK, no client library, no wrapper.** An agent already runs shell commands, and that is
the whole integration:

```sh
sbx create task-4711 --template postgres          # nothing on disk needed
sbx env task-4711 --shell json
# {"DATABASE_HOST":"127.0.0.1","DATABASE_PORT":"20000","SBX_SANDBOX":"task-4711", …}
```

It parses that, connects with whatever tool it was going to use anyway, and the connection
itself is the wake signal. Needs something the spec never declared? It can add one mid-task —
and that service sleeps like the rest and dies with the sandbox, rather than becoming a stray
container that belongs to nobody:

```sh
sbx add task-4711 cache --image redis:7-alpine --port 6379 --health 'redis-cli ping'
```

If the agent runs code *you did not write*, add `"egress": "deny"` and read
[is it for you?](#is-it-for-you) first — a container shares your kernel.

### One seeded database, many agents

Seed once, then hand every branch or agent its own copy. The migration runs once rather than
once per agent, which is what makes a sandbox each affordable:

```sh
sbx create main --template postgres
sbx cp   main postgres ./schema.sql :/tmp/schema.sql           # your own migration file
sbx exec main postgres psql -U app -d app -f /tmp/schema.sql   # seed once
sbx snapshot main golden

sbx fork golden agent-1                                        # as many as you want
sbx fork golden agent-2
```

A write in one is invisible to the others and to the original. ⚠️ **Filesystem state only** —
processes start cold against warm data, and a fork is not a paused process resumed. `sbx
doctor` reports whether this machine could do better. → [USE-CASES.md](docs/USE-CASES.md)

### CI

```sh
sbx serve --idle 30m &
sbx create "$BRANCH" && sbx ready "$BRANCH"   # blocks until it is actually serving
eval "$(sbx env "$BRANCH")"
./run-tests.sh
```

`ready` starts what it needs and waits — asking *is* starting, which is why there is no `up`.
It refuses if nothing answers on the ports `env` exports, rather than reporting a sandbox as
serving on an address that accepts nothing. `sbx prewarm` moves the image pull into a step
your runner can cache, and on a persistent runner leaving the sandbox behind means the next
job reuses warm, migrated state and pays one wake instead of a create.

### A team, on hardware you already have

sbx is a tool you run, not a service anyone sells. The team case is the same binary on a box
you already own:

```sh
sbx serve --idle 30m &
sbx create my-branch --template web-stack
sbx url my-branch web                   # https://….trycloudflare.com, wakes on open
```

⚠️ **A shared box, deliberately — not a multi-tenant platform.** No authentication, no
per-user isolation, no quota, and none planned. Right for a team that already trusts each
other, wrong for anything public. →
[DECISIONS.md](docs/DECISIONS.md)

---

## Commands

| | |
|---|---|
| `sbx create` / `rm` | make one from `sandbox.json` or `--template`, destroy it with its data |
| `sbx env` | exports for your tooling — posix, fish, powershell, **json** |
| `sbx ready` | wake it and block until it's serving — the CI one-liner |
| `sbx exec [-t]` | run anything inside it; `-t` attaches a terminal for a shell or `psql` |
| `sbx logs [-f]` | **every service on one stdout**, structured — the one command that does *not* wake anything |
| `sbx cp` | files in and out (`:` marks the inside path) |
| `sbx add` | drop in a service nobody declared — the agent affordance |
| `sbx url` | a public link that wakes it when opened |
| `sbx snapshot` / `fork` | save every service's data, then make **as many sandboxes from it as you want** |
| `sbx init` | print a starter `sandbox.json` — `sbx init > sandbox.json` |
| `sbx validate` | check it without creating anything — the pre-commit hook |
| `sbx prewarm` | pull the images now, so the first create isn't a download |
| `sbx gc` | reclaim volumes whose sandbox is gone — lists by default, deletes only with `--force` |
| `sbx doctor` | what this machine can and cannot do, before you rely on it |
| `sbx list` · `sbx templates` | what exists and what's awake · the built-in specs |
| `sbx serve` | **the daemon** — it owns the ports and does all waking and sleeping; one per machine |
| `sbx selftest` | the whole cycle, on your machine |

```
INFO [14:16:40] my-branch/postgres  database system is ready to accept connections
INFO [14:16:40] my-branch/redis     Ready to accept connections tcp
```

Aligned columns on a terminal, **JSON when piped**.

Every command that touches a sandbox takes `--provider docker|kubernetes`, `--namespace` and
`--isolation container|gvisor|kata` — `serve` takes the first two. `doctor`, `validate` and
`templates` need none of them. `SBX_PROVIDER_KIND`, `SBX_NAMESPACE` and `SBX_ISOLATION` set
the defaults, and `DOCKER_HOST` is honoured.

**What a sandbox is** is one committed file. → [SPEC.md](docs/SPEC.md)

---

## Is it for you?

**Yes, if** your branches share one database and a migration on one is a migration on all; if
an agent needs somewhere to work that dies with the task; if CI spends longer waiting for a
stack than running tests.

**No, if** you need to run code you did not write — a container shares your kernel, and E2B,
Vercel Sandbox or Modal give you a real one. Or if you want somebody else to operate it:
that's Neon, and always will be.

The one thing that makes this different from every other tool here: **nothing has to call an
SDK to wake a sandbox.** A connection pool can't call `sandbox.connect()`, and neither can
`pg_dump`, a migration tool or a test runner somebody else wrote.

| | wakes on | so the client can be |
|---|---|---|
| **sbx** | **any TCP connection** | anything with a socket |
| E2B · Daytona · Modal · Vercel · Cloudflare | an SDK call | only your own code |
| Fly Machines | a request through Fly Proxy | anything, incl. TCP |
| Knative | an HTTP request | HTTP/gRPC/WS only |

⚠️ **The wake is paid on `connect`, not on the query.** A client that gives up connecting in
two seconds will give up on a cold postgres. Raise its connect timeout above the wake —
`PGCONNECT_TIMEOUT` for libpq — and expect a pooled client to see a server-initiated close
when a sandbox sleeps. → [TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md)

⚠️ **Honest limits.** A container shares the host kernel unless you run `--isolation gvisor`,
which CI proves end to end. Egress can be denied but not filtered by domain. Memory is not
restored, so a wake starts processes cold. And **nobody outside its author has run this in
production** — [what *is* tested](docs/BENCHMARKS.md) is every push on Linux and macOS,
including a real daemon, three concurrent sandboxes, a snapshot/fork cycle and a daemon kill.

→ [COMPARISON.md](docs/COMPARISON.md) for the full field, every claim sourced or measured,
including where we lose.

---

## Docs

| | |
|---|---|
| [SPEC.md](docs/SPEC.md) | every field of `sandbox.json` |
| [USE-CASES.md](docs/USE-CASES.md) | seven shapes this fits, and the ones it doesn't |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | the pieces, both data paths, addressing |
| [COMPARISON.md](docs/COMPARISON.md) | against E2B, Daytona, Modal, Fly, Neon, Knative |
| [BENCHMARKS.md](docs/BENCHMARKS.md) | every number, and the script that produced it |
| [TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) | what to do about what you're seeing |
| [DECISIONS.md](docs/DECISIONS.md) | why it's shaped this way — mostly things that broke |
| [console/](console/) | metrics, health and a read-only API for a running daemon |

MIT. Issues and patches welcome.
