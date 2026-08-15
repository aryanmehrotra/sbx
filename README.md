# sbx

[![CI](https://github.com/aryanmehrotra/sbx/actions/workflows/ci.yaml/badge.svg)](https://github.com/aryanmehrotra/sbx/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8)](go.mod)
[![dependencies](https://img.shields.io/badge/dependencies-0-3fb950)](go.mod)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**One Go binary. Every branch, task or agent gets its own Postgres, Redis or browser — asleep
at 0 B, awake in 191 ms the moment something connects to it.**

<img src="docs/demo.svg" alt="A terminal running sbx selftest: a sandbox is created, sleeps to zero, is woken by a socket in 251 ms, and its data survives." width="860">

```sh
sbx create my-branch --template postgres     # 492 ms, no spec file needed
eval "$(sbx env my-branch --template postgres)"
psql                                         # this wakes it
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

One static binary, **nothing outside Go's standard library**. darwin, linux, windows and
freebsd on amd64 and arm64; Windows means WSL2. Then run the daemon once, supervised —
[`deploy/`](deploy/) has a launchd plist and a systemd unit, both running as you, not root.

Verify it on your own machine in about nine seconds — that's what the demo above is:

```sh
sbx selftest
```

---

## Use it

Five situations, and they differ mostly in *who types the commands*. This is the **how**;
[USE-CASES.md](docs/USE-CASES.md) is the **why** — the problem each solves, and the numbers.

### You, with three branches open

The case it was built for. One spec in the repo, one sandbox per branch:

```sh
git switch feature-x
sbx create feature-x                  # reads ./sandbox.json
eval "$(sbx env feature-x)"           # DATABASE_PORT=20002, REDIS_PORT=20003…
npm test                              # your tooling, unchanged
```

Switch away and it sleeps to 0 B on its own. Switch back and the first query wakes it. You
never type `start` or `stop` because there isn't one.

### An agent, mid-task

**No SDK, no client library, no wrapper.** An agent already runs shell commands, and that is
the whole integration:

```sh
sbx create task-4711 --template postgres    # nothing on disk needed
sbx env task-4711 --template postgres --shell json
```

```json
{
  "DATABASE_HOST": "127.0.0.1",
  "DATABASE_PORT": "20000",
  "SBX_PROVIDER": "docker",
  "SBX_SANDBOX": "task-4711"
}
```

It parses that, connects with whatever tool it was going to use anyway — `psql`, a migration
runner, a test suite — and the connection itself is what wakes the sandbox. Nothing in the
agent's toolchain has to know sbx exists.

Needs something the spec never declared? It can add one mid-task:

```sh
sbx add task-4711 cache --image redis:7-alpine --port 6379 --health 'redis-cli ping'
#   cache        ✓ 127.0.0.1:20001
```

That service sleeps like the rest and is destroyed with the sandbox — rather than a stray
container that outlives the task and belongs to nobody.

Worth saying plainly: if the agent is running code *you did not write*, add
`"egress": "deny"` to the spec, and read [is it for you?](#is-it-for-you)
first — a container shares your kernel, and E2B or Vercel Sandbox give you a real one.

### One seeded database, many agents

Seed it once, then hand every branch or agent its own:

```sh
sbx exec main postgres psql -U app -d app -f schema.sql   # seed once
sbx snapshot main golden                                  # save it

sbx fork golden agent-1                                   # as many as you want
sbx fork golden agent-2
```

Each fork gets its own copy of the data and its own ports. A write in one is invisible to
the others and to the original. It's what makes a sandbox per task affordable: the
migration runs once, not once per agent.

⚠️ **Filesystem state only.** Processes start cold against warm data, exactly as a wake
does — a fork is not a paused process resumed. E2B and zeropod restore memory too, in tens
of milliseconds, and need Firecracker or CRIU to do it. `sbx doctor` tells you whether this
machine has either.

```sh
sbx doctor          # ✗ docker checkpoint  daemon experimental=false
                    #   memory-state sleep is unavailable
```

`doctor` is worth running before you rely on anything: sbx refuses rather than silently
downgrading — asking for `--isolation gvisor` on a host without it fails — and the refusal
shouldn't be the first time you hear about it.

### CI

```sh
sbx serve --idle 30m &                        # fronts the ports `env` exports
sbx create "$BRANCH" && sbx ready "$BRANCH"   # blocks until it is actually serving
eval "$(sbx env "$BRANCH")"
./run-tests.sh
```

`ready` starts what it needs and waits — asking *is* starting, which is why there is no `up`.
It also refuses if nothing is answering on the ports `env` exports, rather than reporting a
sandbox as serving on an address that accepts nothing.

⚠️ **The daemon is not optional here.** `env` exports the *public* ports and `sbx serve` is
what answers on them; without it your tests get a port with nothing behind it. This README
said "no daemon to run" until a use-case test proved otherwise.
On a persistent runner, leaving the sandbox behind is the interesting case: the next job on
that branch reuses warm, migrated state and pays one wake instead of a create.

### Housekeeping

A sandbox's volume outlives it on purpose — that is what makes sleeping safe — but nothing
used to reclaim one after the sandbox was gone, so a machine that has run a sandbox per
branch for a month carries every branch it ever had.

```sh
sbx gc                          # lists what is reclaimable. Deletes nothing
sbx gc --older-than 168h --force
```

It only ever offers artifacts whose sandbox **no longer exists** — a sleeping sandbox is the
normal state here, not garbage. Snapshots are listed but never swept without `--snapshots`,
because outliving their sandbox is the entire point of one.

### A team, on hardware you already have

sbx is a tool you run, not a service anyone sells or offers. The team case is the same
binary on a box you already own:

```sh
sbx serve --idle 30m &                  # deploy/ has a systemd unit and a launchd plist
sbx url my-branch web                   # https://….trycloudflare.com, wakes on open
```

⚠️ **A shared box, deliberately — not a multi-tenant platform.** There is no authentication,
no per-user isolation and no quota, and none are planned: sbx is meant to be adopted into
your workflow, not operated as a service for strangers. It is the right shape for a team that
already trusts each other and the wrong one for anything public. `sbx doctor` tells you what
the host can enforce before you rely on it.

---

## Commands

| | |
|---|---|
| `sbx create` / `rm` | make one from `sandbox.json` or `--template`, destroy it with its data |
| `sbx env` | exports for your tooling — posix, fish, powershell, **json** |
| `sbx ready` | wake it and block until it's serving — the CI one-liner |
| `sbx exec [-t]` | run anything inside it; `-t` attaches a terminal for a shell or `psql` |
| `sbx logs [-f]` | **every service on one stdout**, structured |
| `sbx cp` | files in and out (`:` marks the inside path) |
| `sbx add` | drop in a service nobody declared — the agent affordance |
| `sbx url` | a public link that wakes it when opened |
| `sbx snapshot` | save every service's data under a name |
| `sbx fork` | **a new sandbox from that snapshot** — as many as you want |
| `sbx list` | what exists, what's awake |
| `sbx gc` | reclaim volumes and images whose sandbox is gone — lists by default |
| `sbx prewarm` | pull the images now, so the first create isn't a download — the CI cache step |
| `sbx validate` | check `sandbox.json` without creating anything — the pre-commit hook |
| `sbx doctor` | what this machine can and cannot do, before you rely on it |
| `sbx selftest` | the whole cycle, on your machine |

```
INFO [14:16:40] my-branch/postgres  database system is ready to accept connections
INFO [14:16:40] my-branch/redis     Ready to accept connections tcp
```

Aligned columns on a terminal, **JSON when piped**. Everything wakes what it touches —
**except `logs`**, because asking what a sandbox said isn't using it.

**What a sandbox is** is one committed file. → [SPEC.md](docs/SPEC.md)

---

## Is it for you?

**Yes, if** your branches share one database and a migration on one is a migration on all;
if an agent needs somewhere to work that dies with the task; if CI spends longer waiting for
a stack than running tests.

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
| Lazytainer | packets crossing a threshold | anything that **retries** — 0/5 first attempts served |

→ [COMPARISON.md](docs/COMPARISON.md) for the full field, every claim sourced or measured,
including where we lose.

⚠️ **Honest limits.** A container shares the host kernel unless you run `--isolation gvisor`,
which CI now proves works end to end. Egress can be denied but not filtered by domain. And
**nobody outside its author has run this in production** — [what *is* tested](docs/BENCHMARKS.md)
is every push on Linux and macOS, including a real daemon, three concurrent sandboxes and a
snapshot/fork cycle.

---

## Docs

| | |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | the pieces, both data paths, addressing |
| [SPEC.md](docs/SPEC.md) | every field of `sandbox.json` |
| [USE-CASES.md](docs/USE-CASES.md) | five shapes this fits, and the ones it doesn't |
| [COMPARISON.md](docs/COMPARISON.md) | against E2B, Daytona, Modal, Fly, Neon, Knative |
| [BENCHMARKS.md](docs/BENCHMARKS.md) | every number, and how to re-run it |
| [DECISIONS.md](docs/DECISIONS.md) | why it's shaped this way — mostly things that broke |

MIT. Issues and patches welcome.
