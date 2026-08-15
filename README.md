# sbx

**Give every branch, task or agent its own databases. Pay only for the ones in use.**

```sh
sbx create my-branch --template postgres   # 492 ms, no spec file needed
eval "$(sbx env my-branch --template postgres)"
```

They sleep when nobody's using them and wake when something connects — **191 ms**, no API
call, no `start`, no `stop`.

---

## The problem

```
   TODAY                              WITH sbx
   ─────────────────────────          ─────────────────────────────
   branch A ─┐                        branch A ─▶ own db   ● awake
   branch B ─┼─▶ ONE database         branch B ─▶ own db   ○ 0 B
   branch C ─┘   shared state         branch C ─▶ own db   ○ 0 B
                                                            │
   a migration on one                 nothing shared, and you only
   is a migration on all              pay for what you're looking at
```

The usual fix — a stack per branch — costs full memory for **every branch you ever opened**.
This one costs zero for the ones you're not using.

---

## How it works, in one picture

```
   your client                              sbx serve
   (psql, redis-cli, a pool,             ┌──────────────┐
    Playwright, curl)                    │  always up   │
        │                                │   ~4.5 MB    │
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

**Two ports, because something has to answer while nothing is running.**

---

## Numbers

| | |
|---|---|
| wake, docker | **191 ms** · n=20, p90 232 ms |
| wake, kubernetes | **1534 ms** · n=5 |
| create | **492 ms** |
| a sleeping sandbox | **0 B** |
| the daemon | 4.5 MB |
| tuned MySQL | **110 MB** vs 411 MB stock |

Published, for scale: E2B 662 ms resume · Daytona 1254 ms · Neon 300–500 ms. Those are hosted
platforms. This is a laptop. → [BENCHMARKS.md](docs/BENCHMARKS.md)

---

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | sh
# or
go install github.com/aryanmehrotra/sbx@latest
```

One static binary, **no dependencies outside Go's standard library**. Builds for darwin,
linux, windows and freebsd on amd64 and arm64. Windows means WSL2.

Then run the daemon once, supervised — [`deploy/`](deploy/) has a launchd plist and a systemd
unit. Both run as you, not root.

---

## Prove it here, in about nine seconds

```sh
sbx selftest

  ✓ create a sandbox from a spec     354ms
  ✓ write a value                     31ms
  ✓ sleep to zero when unused       8359ms
  ✓ wake on a plain TCP connection   167ms
  ✓ state survived the sleep            0ms
```

Real provider, real daemon, real client socket. Nothing stubbed.

---

## What you can do with one

| | |
|---|---|
| `sbx create` / `rm` | make one from `sandbox.json`, destroy it with its data |
| `sbx env` | exports for your tooling — posix, fish, powershell, **json** |
| `sbx ready` | wake it and block until it's serving — the CI one-liner |
| `sbx exec` | run anything inside it |
| `sbx logs [-f]` | **all services on one stdout**, structured — a sandbox as a server |
| `sbx cp` | files in and out (`:` marks the inside path) |
| `sbx add` | drop in a service nobody declared — the agent affordance |
| `sbx url` | a public link that wakes it when opened |
| `sbx list` | what exists, what's awake |
| `sbx selftest` | the whole cycle, on your machine |

```
INFO [14:16:40] my-branch/postgres  database system is ready to accept connections
INFO [14:16:40] my-branch/redis     Ready to accept connections tcp
```

Aligned columns on a terminal, **JSON when piped** — which is what a log shipper wants and
what CI captures. `LOG_LEVEL` picks the floor: DEBUG, INFO, NOTICE, WARN, ERROR, FATAL.

Everything wakes what it touches. **Except `logs`** — asking what a sandbox said isn't using
it, and waking three databases because somebody typed `logs` would be the opposite of the
point.

---

## The spec

One file, committed to your repo:

```json
{
  "version": 1,
  "services": {
    "postgres": {
      "image": "postgres:16-alpine",
      "ports": [5432],
      "health": "pg_isready -U app -d app",
      "env": { "POSTGRES_USER": "app", "POSTGRES_PASSWORD": "app", "POSTGRES_DB": "app" },
      "volume": "/var/lib/postgresql/data",
      "init": ["psql -U app -d app -c 'CREATE TABLE IF NOT EXISTS todo (id serial)'"]
    }
  },
  "exports": { "DATABASE_PORT": "postgres:5432" }
}
```

| field | does |
|---|---|
| `health` | how sbx knows it's serving — **runs inside the container** |
| `volume` | what makes sleeping safe: container stopped, data kept |
| `init` | runs **once**, after it first reports healthy — not on every wake |
| `optional` | not created unless `--optional`, but still reserves its ports |
| `exports` | maps onto the variables your scripts already read |

⚠️ **The health command must exist in the image.** A Chrome image with no `wget` can't be
health-checked with `wget`, and the failure looks like a service that never starts:

```sh
docker run --rm --entrypoint sh <image> -c 'command -v wget curl'
```

### Or skip the file entirely

```sh
sbx templates          # analytics  browser  nginx  postgres  web-stack
sbx create my-site --template nginx
```

The templates are the [`examples/`](examples/), embedded in the binary — so an agent asked
to spin up a Postgres can, in one line, with nothing on disk.

---

## Yes, browsers work

```sh
sbx create my-branch --spec examples/browser/sandbox.json
curl "http://$CDP_HOST:$CDP_PORT/json/version"
# {"Browser": "HeadlessChrome/124.0.6367.78", ...}
```

Asleep at 0 B → woken by that request in **624 ms** → then driven over CDP. Anything with a
container image works; anything speaking TCP wakes on demand.

---

## Anywhere

```sh
sbx create my-branch                        # docker, this machine
sbx create my-branch --provider kubernetes  # the same spec, a cluster
```

| | docker | kubernetes |
|---|---|---|
| address | `127.0.0.1:20002` | `sbx-x-pg.sbx.svc:5432` |
| wake | `docker start` | scale → 1 |
| sleep | `docker stop` | scale → 0 |
| health | HEALTHCHECK | readinessProbe |
| storage | named volume | PVC |
| isolation | `--runtime` | `runtimeClassName` |

In a cluster an **activator** holds the port, because a Service selecting the workload can't
answer at zero replicas. It splices bytes rather than parsing a protocol, so Postgres, Redis
and gRPC all work. → [ARCHITECTURE.md](docs/ARCHITECTURE.md)

---

## For agents

An agent mid-task wants a Postgres to try a migration against:

```sh
sbx add my-task pg --image postgres:16-alpine --port 5432 \
  --health "pg_isready -U postgres" --env POSTGRES_PASSWORD=pw
```

It gets a port, sleeps when idle, and dies with the sandbox — instead of a stray container
that outlives the task and belongs to nobody.

**Why an agent can't just use a hosted sandbox for this:** those expose create/pause/resume
through an SDK, so *something has to call resume*. An agent's clients here are `psql`, a
connection pool, a test runner. They can't call an SDK — they open a socket, and the socket
is the wake signal.

---

## Where to use something else

| If you need | Use |
|---|---|
| An agent running genuinely untrusted code | E2B, Daytona |
| A URL per pull request, for reviewers | Uffizzi, Okteto, Northflank |
| Only ephemeral test fixtures | Testcontainers |
| HTTP-only, already on Knative | Knative |

**Honest limits.** A container shares the host kernel — `--isolation gvisor|kata` is
declarable and refused when the runtime is absent, but operating a hardened cluster is yours.
And nobody outside its author has run this in production.

---

## Docs

| | |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | the pieces, both data paths, addressing |
| [USE-CASES.md](docs/USE-CASES.md) | four shapes this fits, and the ones it doesn't |
| [BENCHMARKS.md](docs/BENCHMARKS.md) | every number, and how to re-run it |
| [DECISIONS.md](docs/DECISIONS.md) | why it's shaped this way — mostly things that broke |
