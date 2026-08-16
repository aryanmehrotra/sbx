# The spec

> **Short version:** one committed `sandbox.json` says what services a branch needs. It never
> says when to start or stop them. `sbx init > sandbox.json` gives you a working one;
> `sbx validate` checks it without creating anything.

`sandbox.json` — one file, committed to your repo, describing what a branch needs to run.

A spec never says **when** to start or stop anything. It says what exists, how to tell when it
is serving, and how to reach it. Lifecycle belongs to `sbx serve`, which watches the ports. If
a spec could start something, the spec would eventually be the thing that left it running.

---

## A complete one

```json
{
  "version": 1,
  "services": {
    "postgres": {
      "image": "postgres:16-alpine",
      "ports": [5432],
      "health": "psql -U app -d app -c 'select 1'",
      "env": { "POSTGRES_USER": "app", "POSTGRES_PASSWORD": "app", "POSTGRES_DB": "app" },
      "volume": "/var/lib/postgresql/data",
      "init": ["psql -U app -d app -c 'CREATE TABLE IF NOT EXISTS todo (id serial)'"]
    },
    "redis": {
      "image": "redis:7-alpine",
      "ports": [6379],
      "health": "redis-cli ping"
    },
    "clickhouse": {
      "image": "clickhouse/clickhouse-server:24.3-alpine",
      "ports": [8123],
      "health": "wget -qO- localhost:8123/ping",
      "files": { "clickhouse-low-mem.xml": "/etc/clickhouse-server/config.d/low-mem.xml" },
      "optional": true
    }
  },
  "exports": { "DATABASE_PORT": "postgres:5432", "REDIS_PORT": "redis:6379" }
}
```

⚠️ **Note the postgres health command.** It is `psql … select 1`, not `pg_isready`.
`pg_isready` answers yes while postgres is still bootstrapping — before `POSTGRES_DB` exists —
so `init` would run against a database that is not there yet. Every bundled template uses the
`psql` form for this reason, and so does `sbx init`.

Every image above is shown with a readable tag; the shipped templates additionally pin a
digest, which is what `scripts/pin-templates.sh` maintains.

---

## Top level

| field | required | does |
|---|---|---|
| `version` | ● | `1`. Lets a future format change be detected rather than silently misread |
| `services` | ● | The declared set, keyed by name — a map, so a name is unambiguous when merging |
| `exports` | | Maps port assignments onto the variables your scripts already read — each also yields a matching `_HOST` |

### Every port export gets a host to go with it

A declared export produces two variables, not one. `DATABASE_PORT` also yields
`DATABASE_HOST`; `PGPORT` yields `PGHOST` — no underscore, because that is what libpq itself
reads, and it is why `psql -U app -d app` with no host or port argument reaches the sandbox at
all. `MYSQL_PORT` yields `MYSQL_HOST` by the same rule.

The rule is: strip a trailing `_PORT` if there is one, otherwise a trailing `PORT`, and append
`_HOST` or `HOST` to match. A bare `PORT` export is left alone rather than becoming `_HOST`,
which would be neither useful nor obviously wrong to whoever wrote it.

**`exports` is how adoption stays cheap.** `{"DB_PORT": "mysql:3306"}` becomes
`DB_PORT=<public port of mysql 3306>`. Without it, adopting sbx would mean editing every
script that already knows a port.

---

## Per service

| field | required | does |
|---|---|---|
| `image` | ●* | Any container image |
| `build` | ●* | Build one instead: `{ "context": "./app", "dockerfile": "Dockerfile" }` |
| `ports` | ● | Container-side ports. The public and backing ports are **assigned from the sandbox's slot**, never chosen here |
| `health` | | A command run **inside** the container — how sbx knows it is serving |
| `env` | | Environment variables |
| `args` | | Command arguments, appended to the image's entrypoint |
| `volume` | | One container path to persist. What makes sleeping safe |
| `files` | | Read-only host files, mounted; paths are relative to the spec |
| `init` | | Commands run **once**, after the service first reports healthy |
| `depends_on` | | Services that must be healthy before this one is created |
| `optional` | | Not created unless `--optional` — but still reserves its ports |
| `egress` | | `"deny"` — no routed egress. It can still be reached, and can still talk to its own sandbox |
| `cpu` | | Cores this service may use: `"0.5"`, `"2"`. Unset means unlimited |
| `memory` | | Memory cap: `"512m"`, `"2g"`. Unset means unlimited |
| `gpus` | | Passed to the runtime verbatim: `"all"`, `"1"`, `"device=0"`. Declared rather than inferred, because a sandbox that quietly takes every GPU on a shared machine is a bad neighbour |

### `image` or `build` — exactly one

\* Every service needs something to run. Give it an `image` to pull, or a `build` to make:

```json
{ "build": { "context": "./app" }, "ports": [3000] }
```

`context` is relative to the spec file. `dockerfile` defaults to `Dockerfile` and is relative
to the context. Both is an error rather than a precedence rule — which of the two wins is
exactly the thing a reader guesses wrong, and guessing means running an image the file does
not appear to describe.

**The tag is a hash of the context**, so an unchanged context is a cache hit and no build
runs at all:

```
$ sbx create feat-x            # first time
  web          building…
$ sbx create feat-y            # same context
  web          build cached (sbx-build-bc02342a9ba51b10)
```

**Content, not a clock.** Change one byte, get a different tag. Change nothing, get the same
tag next month.

Three things are deliberately in or out of the hash:

| | |
|---|---|
| **timestamps — out** | a fresh `git clone` rewrites every mtime, so a time-based key misses on every CI runner, which is exactly where the cache is worth most |
| **file modes — in** | a script that stops being executable is a different image, and a silent cache hit there fails at runtime |
| **`.git`, `node_modules` — out** | otherwise every commit and every install busts the cache |

Why not expire it on a timer instead? Because a clock is wrong in both directions at once —
it rebuilds what has not changed and reuses what has.
→ [DECISIONS.md](DECISIONS.md#a-built-image-is-keyed-by-its-content-never-by-its-age)

Docker only. In a cluster, building means pushing to a registry the nodes can pull from —
credentials and a registry sbx has no business assuming — so `sbx create` says so and stops
rather than half-working.

### Why ports aren't yours to choose

Two repos that both picked 5432 collide the moment somebody opens both. Each sandbox gets a
slot, and ports are assigned from it. `exports` is the seam that keeps this invisible to your
tooling.

### `health` is close to required

Without it the daemon can only dial the published port — and Docker answers that before the
server inside does. The first query after a wake then lands on a socket that is about to
close. → [DECISIONS.md](DECISIONS.md#a-published-port-is-not-readiness)

⚠️ **The health command must exist in the image.** A Chrome image with no `wget` cannot be
health-checked with `wget`, and the failure looks like a service that never starts. Check
first:

```sh
docker run --rm --entrypoint sh <image> -c 'command -v wget curl'
```

### `depends_on` orders creation, and nothing else

```json
{ "api":      { "build": { "context": "." }, "ports": [3000], "depends_on": ["postgres"] },
  "postgres": { "image": "postgres:16-alpine", "ports": [5432], "health": "pg_isready -U app" } }
```

Without it, services are created alphabetically — so `api` comes up before `postgres`, and an
app that dials its database at boot fails for a reason that is nowhere in the file. This
mattered much less before `build:` existed, when everything in a spec was a backing service
that waited for nobody.

Two things it deliberately does **not** do:

**It does not change ports.** Ordinals stay alphabetical, so adding `depends_on` to a spec
never moves an existing sandbox's addresses. Somebody's `DATABASE_PORT` changing because a
colleague declared a dependency would be a worse bug than the race it fixes.

**It does not order wakes.** The daemon wakes what is connected to, and after a sleep there is
no "startup" for an ordering rule to attach to. A service that needs another at runtime should
retry — which it has to anyway, because that is what waking looks like from the inside.

A dependency on a service the spec does not declare is refused, rather than being a rule that
silently never applied. So is a cycle.

### `${VAR}` keeps a secret out of a committed file

```json
{ "env": { "POSTGRES_PASSWORD": "${DB_PASSWORD}" } }
```

`sandbox.json` is meant to be committed. For `POSTGRES_PASSWORD: "app"` on a throwaway local
database that is fine and will stay fine; for a private registry credential or a real key some
fixture seeding needs, it means a secret in git. A value in `env` may instead reference the
environment sbx was invoked with.

Deliberately the smallest version of this:

- **`env` values only.** Not images, not health commands, not init steps — expansion inside a
  command is where this stops being substitution and starts being a shell.
- **No defaults or nesting.** No `${VAR:-fallback}`. Each of those is a small syntax nobody
  asked for and everyone has to learn, and your shell already has all of them.
- **An unset variable is an error**, reported before anything is created, listing every
  missing name at once. A database that came up with an empty password because a variable was
  not exported is a failure that looks like success.
- **`${...}` only** — a bare `$NAME` is left alone, so a password containing a dollar sign
  survives.

Anything further — Vault, 1Password, a cloud secret manager — stays out: a dependency, a
network call, and a credential needed to fetch the credential, in a binary whose whole claim
is that it has none of those.

### sbx remembers which spec a sandbox came from

`--template postgres` had to be repeated on `create`, then `env`, then `fork`. Forgetting it
gave you one of two things, and the second is worse: `open sandbox.json: no such file` if the
directory had none, or — if an unrelated `sandbox.json` happened to be there — a clean success
against the wrong spec. Ordinals are assigned alphabetically over the declared service names,
so a different-but-valid spec shifts them and `sbx env` prints a plausible, wrong port.

So sbx writes it down, under `~/.sbx/origins/`, and uses it when nothing was asked for:

```sh
sbx create main --template postgres
sbx env    main                  # no flag needed
sbx snapshot main golden         # the snapshot inherits it
sbx fork   golden agent-1        # and so does the fork
sbx env    agent-1               # still no flag
```

Always a **default**, never a source of truth: an explicit `--spec` or `--template` wins, a
missing or unreadable record changes nothing, and no command fails because of it. The
containers and their labels remain the only place the truth about a sandbox lives. A recorded
path that has since been deleted is ignored rather than used, because sending every later
command at a path that no longer exists is worse than falling back to the working directory.

### Check it without creating it

```sh
sbx validate                    # ./sandbox.json
sbx validate path/to/spec.json
```

Reads the file, resolves ports and ordering, and creates nothing — so a pre-commit hook or a
lint job can check a change to a committed spec without a docker daemon. It runs the same
loader `create` does, which is the only way it is worth having: a separate validator would
drift, and a spec that passes lint and fails create is worse than no lint.

It also names anything it merely dislikes, like a service with no `health`.

### `init` runs once, not on every wake

Schemas, users, seed data. A woken container already has whatever this created, so re-running
it would be at best wasted and at worst destructive.

### `cpu` and `memory` are the ceiling a laptop needs

```json
{ "image": "postgres:16-alpine", "ports": [5432], "cpu": "0.5", "memory": "512m" }
```

Unset means unlimited, which is fine for one sandbox and stops being fine at twenty: a
machine running a sandbox per branch otherwise has no ceiling at all, and the thing that
fails is the machine rather than the sandbox. Docker gets `--cpus`/`--memory`; a cluster gets
`resources.limits`. Requests are deliberately left alone in the cluster case — those change
scheduling, which is the operator's business.

### `egress: "deny"` blocks the way out, not the way in

```json
{ "image": "node:22", "ports": [3000], "egress": "deny" }
```

Docker gets a per-sandbox bridge with IP masquerade disabled: no NAT off the host, so nothing
routed leaves — and docker still publishes ports into it, so waking is untouched. Verified
both directions: the service could not fetch `example.com`, and it still woke on a connection
and answered 200.

The obvious alternatives do not work and were tried: `--internal` and `--network none` both
block egress **and** stop docker publishing the port, producing a sandbox that can never be
woken. → [DECISIONS.md](DECISIONS.md)

It is **not a domain allow-list**. Docker has no primitive for that; doing it properly needs a
filtering proxy in the data path, which is a component with a lifecycle rather than a flag.
DNS still resolves — docker's resolver sits on the bridge and needs no route out.

The kubernetes provider **refuses** a service that declares it rather than starting one with
egress open: the cluster answer is a NetworkPolicy, only some CNIs enforce them, and a
security control that silently did nothing is worse than one that says no.

### `optional` still reserves its ports

A branch that never queries the analytics store shouldn't pay for one. But the ports stay
reserved, so adding it later doesn't renumber everything else.
→ [DECISIONS.md](DECISIONS.md#optional-services-still-reserve-their-ports)

---

## Or skip the file entirely

```sh
sbx templates                             # analytics browser nginx postgres web-stack
sbx create my-site --template nginx
```

The templates are the [`examples/`](../examples/), embedded in the binary — so an agent asked
to spin up a Postgres can do it in one line, with nothing on disk.

---

## Coming from docker-compose

Most teams already declare their backing services in a `docker-compose.yml`, and a compose
service is close to isomorphic to an sbx one. The mapping, field for field:

| docker-compose | sandbox.json | note |
|---|---|---|
| `image` | `image` | the same, and pin it |
| `build.context` / `build.dockerfile` | `build.context` / `build.dockerfile` | sbx tags by a hash of the context |
| `ports: ["5432:5432"]` | `ports: [5432]` | **container side only** — the host side is assigned from the sandbox's slot |
| `environment` | `env` | `${VAR}` works in both |
| `command` | `args` | appended to the image's entrypoint |
| `volumes: ["pgdata:/var/lib/postgresql/data"]` | `volume: "/var/lib/postgresql/data"` | one per service, named after the sandbox |
| `volumes: ["./my.conf:/etc/my.conf:ro"]` | `files: {"./my.conf": "/etc/my.conf"}` | read-only, relative to the spec |
| `healthcheck.test` | `health` | a shell command, run inside the container |
| `depends_on` | `depends_on` | ordering only, as in compose without a condition |
| `deploy.resources.limits` | `cpu`, `memory` | |
| `profiles` | `optional` | created with `--optional` |
| — | `exports` | the piece compose has no equivalent of, and the reason adoption is cheap |

**The one that surprises people is `ports`.** Compose lets you choose the host port; sbx does
not, because two repos that both chose 5432 collide the moment somebody opens both. You
declare the container port and read the assigned one back through `exports`.

A two-service compose file:

```yaml
services:
  db:
    image: postgres:16-alpine
    ports: ["5432:5432"]
    environment: { POSTGRES_USER: app, POSTGRES_PASSWORD: app, POSTGRES_DB: app }
    volumes: [ "pgdata:/var/lib/postgresql/data" ]
    healthcheck: { test: ["CMD-SHELL", "pg_isready -U app"] }
  cache:
    image: redis:7-alpine
    ports: ["6379:6379"]
```

becomes:

```json
{
  "version": 1,
  "services": {
    "db": {
      "image": "postgres:16-alpine",
      "ports": [5432],
      "env": { "POSTGRES_USER": "app", "POSTGRES_PASSWORD": "app", "POSTGRES_DB": "app" },
      "volume": "/var/lib/postgresql/data",
      "health": "pg_isready -U app"
    },
    "cache": { "image": "redis:7-alpine", "ports": [6379], "health": "redis-cli ping" }
  },
  "exports": { "DATABASE_PORT": "db:5432", "REDIS_PORT": "cache:6379" }
}
```

Anything reading `DATABASE_PORT` keeps working; `docker compose up` had it on 5432 and sbx
has it wherever this sandbox's slot puts it.

**What does not carry over**: compose's `networks` (each sandbox gets its own), `restart`
(the daemon owns lifecycle — there is no start and no stop), and `depends_on` *conditions*
(sbx waits for health before creating a dependent, which is the `service_healthy` behaviour;
there is no other condition to choose).

`sbx validate` checks the result without creating anything.
