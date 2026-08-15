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

## Why

```
   TODAY                              WITH sbx
   ─────────────────────────          ─────────────────────────────
   branch A ─┐                        branch A ─▶ own db   ● awake
   branch B ─┼─▶ ONE database         branch B ─▶ own db   ○ 0 B
   branch C ─┘   shared state         branch C ─▶ own db   ○ 0 B

   a migration on one                 nothing shared, and you only
   is a migration on all              pay for what you're looking at
```

The usual fix — a stack per branch — costs full memory for **every branch you ever opened**.
This costs zero for the ones you're not using. → [USE-CASES.md](docs/USE-CASES.md)

---

## How it works

```
   your client                              sbx serve
   (psql, redis-cli, a pool,             ┌──────────────┐
    Playwright, curl)                    │  always up   │
        │                                │   ~9.1 MB    │
        │  :20002  ── PUBLIC ────────────▶              │
        │            (owned by sbx)      └──────┬───────┘
        │                                       │
        │                              ┌────────▼────────┐
        │                              │ start it,       │
        │                              │ wait for ready  │
        │                              └────────┬────────┘
        │                                       │
        │                          :30002 ── BACKING ──▶ ┌──────────┐
        │                          (only exists          │ postgres │
        │                           while awake)         │  :5432   │
        └◀────────────── bytes spliced ──────────────────┴──────────┘
```

**Two ports, because something has to answer while nothing is running.** It splices bytes
rather than parsing a protocol, which is why Postgres, Redis, gRPC and CDP all work
unchanged. The same spec runs on Docker or Kubernetes.
→ [ARCHITECTURE.md](docs/ARCHITECTURE.md)

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

## Numbers

| | |
|---|---|
| wake, docker | **191 ms** · n=20, p90 232 ms |
| wake, kubernetes | **1534 ms** · n=5 |
| a sleeping sandbox | **0 B** |
| the daemon | **9.1 MB** at rest · 9.6 MB fronting one sandbox |

Measured by [`scripts/bench.sh`](scripts/bench.sh), on the machine printed beside the results.
→ [BENCHMARKS.md](docs/BENCHMARKS.md)

---

## One seeded database, many copies

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

---

## How it compares

Two different products get called "sandbox". Most are **a place to run code**, where the
client is an SDK and isolation is what's sold. This is **a place to run the services a branch
needs**, where the client is `psql` and cost at rest is what's sold.

Everyone scales to zero. What separates them is **what has to happen to bring it back** —
which decides who is allowed to be the client:

| | wakes on | so the client can be | off-cloud |
|---|---|---|---|
| **sbx** | **any TCP connection** | anything with a socket | **laptop + cluster** |
| E2B · Daytona · Modal · Vercel | an SDK call | only your own code | ✗ |
| Cloudflare Sandbox SDK | an SDK call over RPC | only your own code | ✗ |
| Fly Machines | a request through Fly Proxy | anything, incl. TCP | ✗ |
| Knative | an HTTP request | HTTP/gRPC/WS only | ✓ cluster |
| Neon | a Postgres connection | Postgres clients only | ✗ |
| zeropod | any TCP connection | anything with a socket | ✓ CRIU + eBPF shim, per node |
| Lazytainer | packets on an interface | anything with a socket | ✓ owns your networking |
| Sablier | an HTTP request via a proxy | HTTP only | ✓ |

The last three are self-hosted Go projects doing the same job, and they're the ones to read
before this one — the bottom half of that table is the honest competition, not the top.

**What you give up.** E2B and zeropod both restore a **memory** snapshot: processes and
variables come back exactly as they were. This restores disk only, so a wake is a cold process
start against warm data. Worse guarantee — and it's why a sleeping sandbox here is 0 B rather
than "storage only", and why sleeping costs nothing to enter.

→ [COMPARISON.md](docs/COMPARISON.md) — every claim sourced to vendor docs, plus the chart
where we sit fast **and narrow**, and when to use E2B, Northflank, Testcontainers or Neon
instead.

⚠️ **Honest limits.** A container shares the host kernel; `--isolation gvisor|kata` is
declarable and fails closed when absent, but operating a hardened cluster is yours. Every
hosted platform above has real isolation and real users. **Nobody outside its author has run
this in production.**

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
